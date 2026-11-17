package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/openimsdk/chat/internal/api/util"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/redis/go-redis/v9"
)

// memoryGroupReadLimiter stands in for Redis: one notice per key until reset.
type memoryGroupReadLimiter struct {
	mu   sync.Mutex
	seen map[string]bool
	err  error
}

func (l *memoryGroupReadLimiter) allow(_ context.Context, key string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return false, l.err
	}
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	if l.seen[key] {
		return false, nil
	}
	l.seen[key] = true
	return true, nil
}

func (l *memoryGroupReadLimiter) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = nil
}

func groupReadTestAPI(imURL string, limiter groupReadLimiter) *Api {
	return &Api{
		Api:              &util.Api{ImUserID: "imAdmin"},
		extrasIMURL:      imURL,
		imApiCaller:      testMessageExtrasCaller{},
		groupReadLimiter: limiter,
	}
}

type recordedNotification struct {
	businessNotificationReq
	notice groupReadNotice
}

// groupReadIMServer answers pull_msg_by_seq with msgs and records every business notification.
func groupReadIMServer(t *testing.T, msgs []*sdkws.MsgData) (*httptest.Server, func() []recordedNotification) {
	t.Helper()
	var mu sync.Mutex
	var sent []recordedNotification
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/msg/pull_msg_by_seq":
			var req sdkws.PullMessageBySeqsReq
			if err := json.Unmarshal(body, &req); err != nil || len(req.SeqRanges) != 1 {
				t.Errorf("pull request %s", body)
			}
			data, _ := json.Marshal(&sdkws.PullMessageBySeqsResp{Msgs: map[string]*sdkws.PullMsgs{req.SeqRanges[0].ConversationID: {Msgs: msgs}}})
			_, _ = w.Write([]byte(`{"errCode":0,"data":` + string(data) + `}`))
		case "/msg/send_business_notification":
			var req businessNotificationReq
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("notification request %s", body)
			}
			var notice groupReadNotice
			_ = json.Unmarshal([]byte(req.Data), &notice)
			mu.Lock()
			sent = append(sent, recordedNotification{req, notice})
			mu.Unlock()
			_, _ = w.Write([]byte(`{"errCode":0,"data":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, func() []recordedNotification {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedNotification(nil), sent...)
	}
}

func TestGroupReadNotifiesSendersOfReadMessagesOnly(t *testing.T) {
	server, sent := groupReadIMServer(t, []*sdkws.MsgData{
		{SendID: "alice", GroupID: "g1", Seq: 8, ContentType: 101, Status: 2},
		{SendID: "alice", GroupID: "g1", Seq: 9, ContentType: 101, Status: 2},
		{SendID: "bob", GroupID: "g1", Seq: 10, ContentType: 101, Status: 2},
		{SendID: "reader", GroupID: "g1", Seq: 11, ContentType: 101, Status: 2},  // the reader's own
		{SendID: "system", GroupID: "g1", Seq: 12, ContentType: 1501, Status: 2}, // a notification
		{SendID: "carol", GroupID: "g1", Seq: 13, ContentType: 101, Status: 4},   // deleted
	})
	api := groupReadTestAPI(server.URL, &memoryGroupReadLimiter{})
	err := api.notifyGroupReadProgress(context.Background(), groupReadWebhookReq{SendID: "reader", ConversationID: "sg_g1", GroupID: "g1", HasReadSeq: 13})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]groupReadNotice{}
	for _, n := range sent() {
		if n.Key != groupReadNotificationKey || n.ReliabilityLevel != 1 || n.SendMsg || n.SendUserID != "imAdmin" {
			t.Fatalf("notification %+v", n.businessNotificationReq)
		}
		got[n.RecvUserID] = n.notice
	}
	if len(got) != 2 || got["alice"].HasReadSeq != 13 || got["bob"].ReaderID != "reader" || got["bob"].ConversationID != "sg_g1" {
		t.Fatalf("notified %+v", got)
	}
}

func TestGroupReadThrottlesEachSenderPerConversation(t *testing.T) {
	server, sent := groupReadIMServer(t, []*sdkws.MsgData{{SendID: "alice", GroupID: "g1", Seq: 5, ContentType: 101, Status: 2}})
	limiter := &memoryGroupReadLimiter{}
	api := groupReadTestAPI(server.URL, limiter)
	req := groupReadWebhookReq{SendID: "reader", ConversationID: "sg_g1", GroupID: "g1", HasReadSeq: 5}
	for i := 0; i < 2; i++ {
		if err := api.notifyGroupReadProgress(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(sent()); n != 1 {
		t.Fatalf("sent %d notifications within the interval, want 1", n)
	}
	limiter.reset() // the Redis key expired
	if err := api.notifyGroupReadProgress(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := len(sent()); n != 2 {
		t.Fatalf("sent %d notifications after the interval, want 2", n)
	}
}

func TestGroupReadStillNotifiesWhenTheThrottleFails(t *testing.T) {
	server, sent := groupReadIMServer(t, []*sdkws.MsgData{{SendID: "alice", GroupID: "g1", Seq: 5, ContentType: 101, Status: 2}})
	api := groupReadTestAPI(server.URL, &memoryGroupReadLimiter{err: errors.New("redis down")})
	if err := api.notifyGroupReadProgress(context.Background(), groupReadWebhookReq{SendID: "reader", ConversationID: "sg_g1", GroupID: "g1", HasReadSeq: 5}); err != nil {
		t.Fatal(err)
	}
	if n := len(sent()); n != 1 {
		t.Fatalf("sent %d notifications, want 1", n)
	}
}

func TestGroupReadThrottleUsesRedisWithExpiry(t *testing.T) {
	limiter := redisGroupReadLimiter{rdb: redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond})}
	defer limiter.rdb.Close()
	if _, err := limiter.allow(context.Background(), "sg_g1:alice"); err == nil {
		t.Fatal("an unreachable Redis must report an error, not allow or deny silently")
	}
}

func TestOpenIMWebhookRequiresSecretAndIgnoresOtherCommands(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OPENIM_WEBHOOK_SECRET", "s3cret")
	router := gin.New()
	router.POST("/callback/openim_webhook/:secret/:command", (&Api{}).OpenIMWebhook)
	// With message extras disabled (no IM URL, no Redis) a valid read is acknowledged and dropped.
	post := func(path, body string) int {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return w.Code
	}
	if code := post("/callback/openim_webhook/wrong/"+groupReadWebhookCommand, `{}`); code != http.StatusNotFound {
		t.Fatalf("wrong secret answered %d", code)
	}
	if code := post("/callback/openim_webhook/s3cret/callbackAfterSendGroupMsgCommand", `{}`); code != http.StatusOK {
		t.Fatalf("other command answered %d", code)
	}
	if code := post("/callback/openim_webhook/s3cret/"+groupReadWebhookCommand, `{"sendID":"r","conversationID":"sg_g1","groupID":"g1","hasReadSeq":3}`); code != http.StatusOK {
		t.Fatalf("read with extras disabled answered %d", code)
	}
	// Invalid payloads are acknowledged but not processed.
	if code := post("/callback/openim_webhook/s3cret/"+groupReadWebhookCommand, `{"sendID":"r","conversationID":"sg_other","groupID":"g1","hasReadSeq":3}`); code != http.StatusOK {
		t.Fatalf("mismatched conversation answered %d", code)
	}
	t.Setenv("OPENIM_WEBHOOK_SECRET", "")
	if code := post("/callback/openim_webhook//"+groupReadWebhookCommand, `{}`); code != http.StatusNotFound {
		t.Fatalf("unconfigured secret answered %d", code)
	}
}

func TestValidGroupRead(t *testing.T) {
	for _, tc := range []struct {
		req  groupReadWebhookReq
		want bool
	}{
		{groupReadWebhookReq{SendID: "r", ConversationID: "sg_g", GroupID: "g", HasReadSeq: 1}, true},
		{groupReadWebhookReq{SendID: "", ConversationID: "sg_g", GroupID: "g", HasReadSeq: 1}, false},
		{groupReadWebhookReq{SendID: "r", ConversationID: "si_a_b", GroupID: "g", HasReadSeq: 1}, false},
		{groupReadWebhookReq{SendID: "r", ConversationID: "sg_g", GroupID: "g", HasReadSeq: 0}, false},
	} {
		if got := validGroupRead(tc.req); got != tc.want {
			t.Errorf("validGroupRead(%+v) = %v", tc.req, got)
		}
	}
}
