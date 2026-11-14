package chat

import (
	"context"
	"fmt"
	"github.com/gin-gonic/gin"
	chatconstant "github.com/openimsdk/chat/pkg/common/constant"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/chat/pkg/common/rtcauthority"
	"github.com/openimsdk/protocol/constant"
	"github.com/openimsdk/tools/mw"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookWithoutOperationHeaderStillRequiresSignature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.ContextWithFallback = true
	r.Use(rtcWebhookOperationID, mw.GinParseOperationID())
	api := Api{rtcAuthority: &rtcauthority.Service{Key: "test", Secret: "test-secret"}}
	r.POST("/rtc/webhook", api.RTCWebhook)
	req := httptest.NewRequest(http.MethodPost, "/rtc/webhook", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook was not rejected: %d", w.Code)
	}
	if req.Header.Get(constant.OperationID) == "" {
		t.Fatal("trace id not created")
	}
}
func TestBackgroundRTCOperationContext(t *testing.T) {
	ctx := rtcOperationContext(context.Background())
	if ctx.Value(constant.OperationID) == nil {
		t.Fatal("background request lacks trace")
	}
	existing := context.WithValue(context.Background(), constant.OperationID, "existing")
	if rtcOperationContext(existing).Value(constant.OperationID) != "existing" {
		t.Fatal("lost caller trace")
	}
}

func TestRTCCallRejectsBodyOver16KiBBeforeAuthority(t *testing.T) {
	const limit = 16 << 10
	store := &countingRTCStateStore{}
	service := &rtcauthority.Service{Store: store}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.ContextWithFallback = true
	api := &Api{rtcAuthority: service}
	r.POST("/user/rtc/call", func(c *gin.Context) {
		c.Request = c.Request.WithContext(mctx.WithOpUserID(c.Request.Context(), "qa-user", chatconstant.NormalUser))
		api.RTCCall(c)
	})
	prefix, suffix := `{"action":"list","padding":"`, `"}`
	body := prefix + strings.Repeat("x", limit-len(prefix)-len(suffix)+1) + suffix
	req := httptest.NewRequest(http.MethodPost, "/user/rtc/call", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `"errCode":0`) {
		t.Fatalf("oversized RTC request unexpectedly succeeded: %s", w.Body.String())
	}
	if store.calls != 0 {
		t.Fatalf("oversized request reached authority %d times", store.calls)
	}
}

func TestRTCCallAllowsBodyAt16KiB(t *testing.T) {
	const limit = 16 << 10
	store := &countingRTCStateStore{}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.ContextWithFallback = true
	api := &Api{rtcAuthority: &rtcauthority.Service{Store: store}}
	r.POST("/user/rtc/call", func(c *gin.Context) {
		c.Request = c.Request.WithContext(mctx.WithOpUserID(c.Request.Context(), "qa-user", chatconstant.NormalUser))
		api.RTCCall(c)
	})
	prefix, suffix := `{"action":"list","padding":"`, `"}`
	body := prefix + strings.Repeat("x", limit-len(prefix)-len(suffix)) + suffix
	req := httptest.NewRequest(http.MethodPost, "/user/rtc/call", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `"errCode":0`) {
		t.Fatalf("body at RTC limit failed: %s", w.Body.String())
	}
	if store.calls == 0 {
		t.Fatal("request at body limit did not reach authority")
	}
}

type countingRTCStateStore struct{ calls int }

func (s *countingRTCStateStore) Update(_ context.Context, apply func(*rtcauthority.State) error) error {
	s.calls++
	return apply(&rtcauthority.State{Calls: map[string]*rtcauthority.Call{}})
}

func TestRTCRequestFieldLimits(t *testing.T) {
	cases := []struct {
		name, action, room, peer, groupID, requestID string
		max                                          int
	}{
		{"action", strings.Repeat("a", 33), "", "", "", "", 32},
		{"room", "list", strings.Repeat("r", 257), "", "", "", 256},
		{"peer", "create", "", strings.Repeat("p", 257), "", "", 256},
		{"groupID", "create", "", "", strings.Repeat("g", 257), "", 256},
		{"requestID", "create", "", "", "", strings.Repeat("i", 129), 128},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRTCRequest(rtcauthority.Request{Action: tc.action, Room: tc.room, Peer: tc.peer, GroupID: tc.groupID, RequestID: tc.requestID}); err == nil {
				t.Fatal("expected oversized field rejection")
			}
		})
	}
}

func TestRTCRequestFieldsAtLimitsAreAccepted(t *testing.T) {
	if err := validateRTCRequest(rtcauthority.Request{
		Action: strings.Repeat("a", 32), Room: strings.Repeat("r", 256), Peer: strings.Repeat("p", 256),
		GroupID: strings.Repeat("g", 256), RequestID: strings.Repeat("i", 128),
	}); err != nil {
		t.Fatalf("request fields at byte limits rejected: %v", err)
	}
}

func TestRTCCallRejectsOverlongFieldsBeforeAuthority(t *testing.T) {
	cases := []string{
		`{"action":"` + strings.Repeat("a", 33) + `"}`,
		`{"action":"list","room":"` + strings.Repeat("r", 257) + `"}`,
		`{"action":"create","peer":"` + strings.Repeat("p", 257) + `"}`,
		`{"action":"create","groupID":"` + strings.Repeat("g", 257) + `"}`,
		`{"action":"create","requestID":"` + strings.Repeat("i", 129) + `"}`,
	}
	for i, body := range cases {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			store := &countingRTCStateStore{}
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.ContextWithFallback = true
			api := &Api{rtcAuthority: &rtcauthority.Service{Store: store}}
			r.POST("/user/rtc/call", func(c *gin.Context) {
				c.Request = c.Request.WithContext(mctx.WithOpUserID(c.Request.Context(), "qa-user", chatconstant.NormalUser))
				api.RTCCall(c)
			})
			req := httptest.NewRequest(http.MethodPost, "/user/rtc/call", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if store.calls != 0 {
				t.Fatalf("overlong request reached authority %d times", store.calls)
			}
		})
	}
}

func TestRTCTokenRejectsBodyOver16KiBBeforeAuthority(t *testing.T) {
	store := &countingRTCStateStore{}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.ContextWithFallback = true
	api := &Api{rtcAuthority: &rtcauthority.Service{Store: store}}
	r.POST("/user/rtc/get_token", func(c *gin.Context) {
		c.Request = c.Request.WithContext(mctx.WithOpUserID(c.Request.Context(), "qa-user", chatconstant.NormalUser))
		api.RTCToken(c)
	})
	prefix, suffix := `{"room":"imcall-test","padding":"`, `"}`
	body := prefix + strings.Repeat("x", (16<<10)-len(prefix)-len(suffix)+1) + suffix
	req := httptest.NewRequest(http.MethodPost, "/user/rtc/get_token", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if store.calls != 0 {
		t.Fatalf("oversized token request reached authority %d times", store.calls)
	}
}

func TestRTCTokenRejectsOverlongRoomBeforeAuthority(t *testing.T) {
	store := &countingRTCStateStore{}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.ContextWithFallback = true
	api := &Api{rtcAuthority: &rtcauthority.Service{Store: store}}
	r.POST("/user/rtc/get_token", func(c *gin.Context) {
		c.Request = c.Request.WithContext(mctx.WithOpUserID(c.Request.Context(), "qa-user", chatconstant.NormalUser))
		api.RTCToken(c)
	})
	body := `{"room":"` + strings.Repeat("r", 257) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/user/rtc/get_token", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if store.calls != 0 {
		t.Fatalf("overlong token room reached authority %d times", store.calls)
	}
}
