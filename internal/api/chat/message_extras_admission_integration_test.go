package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/openimsdk/chat/pkg/common/constant"
	"github.com/redis/go-redis/v9"
)

func TestMessageExtrasConcurrentReadsBySameActorQueueInsteadOfReject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	entered := 0
	imServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/conversation/get_conversation" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			ConversationID string `json:"conversationID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode conversation request: %v", err)
		}
		mu.Lock()
		entered++
		n := entered
		mu.Unlock()
		if n == 1 {
			close(firstEntered)
		}
		select {
		case <-releaseFirst:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errCode":0,"data":{"conversation":{"ownerUserID":"alice","conversationID":"` + req.ConversationID + `","minSeq":1,"maxSeq":10}}}`))
	}))
	defer imServer.Close()
	api := &Api{extrasRedis: redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}), extrasIMURL: imServer.URL, imApiCaller: testMessageExtrasCaller{}}
	defer api.extrasRedis.Close()

	request := func(conversationID string) *httptest.ResponseRecorder {
		body := `{"action":"get","conversationID":"` + conversationID + `","messageIDs":["m1"]}`
		req := httptest.NewRequest(http.MethodPost, "/message_extras", strings.NewReader(body))
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = req
		ctx.Set(constant.RpcOpUserID, "alice")
		ctx.Set(constant.RpcOpUserType, []string{"1"})
		api.MessageExtras(ctx)
		return recorder
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- request("sg_first") }()
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first upstream request did not start")
	}
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- request("sg_second") }()
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first upstream request stalled")
	}
	// A second request for the same actor must be admitted while the first is in flight.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := entered
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			close(releaseFirst)
			<-firstDone
			got := <-secondDone
			if strings.Contains(got.Body.String(), "繁忙") {
				t.Fatalf("concurrent same-actor read rejected as busy: %s", got.Body.String())
			}
			t.Fatal("second upstream request was not admitted while first was in flight")
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(releaseFirst)
	for i, result := range []<-chan *httptest.ResponseRecorder{firstDone, secondDone} {
		select {
		case recorder := <-result:
			if strings.Contains(recorder.Body.String(), "繁忙") {
				t.Fatalf("request %d rejected as busy: %s", i+1, recorder.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("request %d did not finish", i+1)
		}
	}
}

func TestMessageExtrasSaturatedSlotsHonorRequestCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if len(extrasSlots) != 0 {
		t.Fatalf("extras slots unexpectedly occupied before test: %d", len(extrasSlots))
	}
	for i := 0; i < cap(extrasSlots); i++ {
		extrasSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(extrasSlots) > 0 {
			<-extrasSlots
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := `{"action":"get","conversationID":"sg_cancel","messageIDs":["m1"]}`
	req := httptest.NewRequest(http.MethodPost, "/message_extras", strings.NewReader(body)).WithContext(ctx)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = req
	ginCtx.Set(constant.RpcOpUserID, "alice")
	ginCtx.Set(constant.RpcOpUserType, []string{"1"})
	api := &Api{}
	done := make(chan struct{})
	go func() {
		api.MessageExtras(ginCtx)
		close(done)
	}()
	// The slots are already full, so this request must be waiting at admission.
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request did not return after its context was canceled")
	}
	if !strings.Contains(recorder.Body.String(), "繁忙") {
		t.Fatalf("canceled waiter response=%s, want admission timeout", recorder.Body.String())
	}
	if got := len(extrasSlots); got != cap(extrasSlots) {
		t.Fatalf("canceled waiter changed occupied slots: got %d, want %d", got, cap(extrasSlots))
	}
}

func TestMessageExtrasDeadlineWaiterDoesNotLeakSlotAfterRelease(t *testing.T) {
	if len(extrasSlots) != 0 {
		t.Fatalf("extras slots unexpectedly occupied before test: %d", len(extrasSlots))
	}
	for i := 0; i < cap(extrasSlots); i++ {
		extrasSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(extrasSlots) > 0 {
			<-extrasSlots
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := acquireProgressSlot(ctx, extrasSlots); err == nil {
		t.Fatal("deadline waiter acquired a slot while all slots were occupied")
	}
	if got := len(extrasSlots); got != cap(extrasSlots) {
		t.Fatalf("deadline waiter changed occupied slots: got %d, want %d", got, cap(extrasSlots))
	}
	for len(extrasSlots) > 0 {
		<-extrasSlots
	}
	if err := acquireProgressSlot(context.Background(), extrasSlots); err != nil {
		t.Fatalf("slot unavailable after timed-out waiter and release: %v", err)
	}
	if got := len(extrasSlots); got != 1 {
		t.Fatalf("fresh acquisition occupancy=%d, want 1", got)
	}
	<-extrasSlots
}
