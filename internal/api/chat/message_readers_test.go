package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	chatconstant "github.com/openimsdk/chat/pkg/common/constant"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/chat/pkg/common/rtcauthority"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/openimsdk/tools/apiresp"
	"github.com/openimsdk/tools/errs"
)

func TestGroupReadersPermissionsAndProgress(t *testing.T) {
	for _, tc := range []struct {
		name, actor, sender string
		outsider, broken    bool
		fail                bool
	}{
		{"author", "owner", "owner", false, false, false},
		{"not author", "owner", "other", false, false, true},
		{"nonmember", "owner", "owner", true, false, true},
		{"upstream unavailable", "owner", "owner", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				var data any
				switch r.URL.Path {
				case "/conversation/get_conversation":
					data = map[string]any{"conversation": map[string]any{"ownerUserID": tc.actor, "groupID": "group", "conversationID": "sg_group"}}
				case "/group/get_group_members_info":
					members := []any{}
					if !tc.outsider {
						members = append(members, map[string]any{"userID": tc.actor})
					}
					data = map[string]any{"members": members}
				case "/msg/pull_msg_by_seq":
					data = map[string]any{"msgs": map[string]any{"sg_group": map[string]any{"Msgs": []any{map[string]any{"clientMsgID": "message", "seq": 5, "sendID": tc.sender, "groupID": "group", "sendTime": 1700000000000, "contentType": 101}}}}}
				case "/group/get_group_member_list":
					data = map[string]any{"total": 4, "members": []any{map[string]any{"userID": "owner", "joinTime": 1699999999000}, map[string]any{"userID": "read", "nickname": "Reader", "joinTime": 1699999999000}, map[string]any{"userID": "unread", "joinTime": 1700000000000}, map[string]any{"userID": "later", "joinTime": 1700000000001}}}
				case "/msg/get_conversations_has_read_and_max_seq":
					if tc.broken {
						_, _ = w.Write([]byte(`{"errCode":500,"errMsg":"offline"}`))
						return
					}
					seq := 4
					if req["userID"] == "read" {
						seq = 5
					}
					data = map[string]any{"seqs": map[string]any{"sg_group": map[string]any{"hasReadSeq": seq}}}
				default:
					t.Errorf("unexpected API %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"errCode": 0, "data": data})
			}))
			defer server.Close()
			api := Api{extrasIMURL: server.URL, imApiCaller: extrasGroupCheckCaller{groups: []*sdkws.GroupInfo{{GroupID: "group"}}}}
			got, err := api.groupMessageReaders(context.Background(), tc.actor, messageReadersRequest{ConversationID: "sg_group", MessageID: "message", Seq: 5})
			if (err != nil) != tc.fail {
				t.Fatalf("got error %v want fail %v", err, tc.fail)
			}
			if !tc.fail && (got.ReadCount != 1 || got.UnreadCount != 1 || len(got.Members) != 2 || got.ExcludedJoinedLater != 1) {
				t.Fatalf("wrong summary %+v", got)
			}
			if !tc.fail && tc.name == "author" {
				sameMillisecondIncluded := false
				for _, member := range got.Members {
					if member.UserID == "unread" {
						sameMillisecondIncluded = true
					}
				}
				if !sameMillisecondIncluded {
					t.Fatalf("member joining at the message millisecond should remain eligible: %+v", got)
				}
			}
		})
	}
}

const messageReadersBodyLimit = 4096

type messageReadersHandlerCaller struct {
	testMessageExtrasCaller
	tokenCalls atomic.Int32
	tokenErr   error
}

func (c *messageReadersHandlerCaller) ImAdminTokenWithDefaultAdmin(context.Context) (string, error) {
	c.tokenCalls.Add(1)
	if c.tokenErr != nil {
		return "", c.tokenErr
	}
	return "test-admin-token", nil
}

func serveMessageReadersHandler(t *testing.T, api *Api, body string, authenticated bool) *apiresp.ApiResponse {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.ContextWithFallback = true
	r.POST("/user/message/readers", api.MessageReaders)
	req := httptest.NewRequest(http.MethodPost, "/user/message/readers", strings.NewReader(body))
	if authenticated {
		req = req.WithContext(mctx.WithOpUserID(req.Context(), "qa-user", chatconstant.NormalUser))
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var response apiresp.ApiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode message readers response %q: %v", w.Body.String(), err)
	}
	return &response
}

func paddedMessageReadersBody(size int) string {
	prefix := `{"conversationID":"sg_test","messageID":"message","seq":5,"padding":"`
	suffix := `"}`
	return prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
}

func TestMessageReadersHandlerRequiresAuthenticatedUserBeforeOpenIM(t *testing.T) {
	caller := &messageReadersHandlerCaller{}
	api := &Api{extrasIMURL: "http://unused.invalid", imApiCaller: caller}
	response := serveMessageReadersHandler(t, api, `{"conversationID":"sg_test","messageID":"message","seq":5}`, false)
	if response.ErrCode != errs.ErrNoPermission.Code() {
		t.Fatalf("unauthenticated response errCode = %d, want %d", response.ErrCode, errs.ErrNoPermission.Code())
	}
	if got := caller.tokenCalls.Load(); got != 0 {
		t.Fatalf("unauthenticated request reached OpenIM admin token %d times", got)
	}
}

func TestMessageReadersHandlerEnforcesFourKiBBodyLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		code int
	}{
		{name: "at limit accepted", size: messageReadersBodyLimit, code: errs.ErrInternalServer.Code()},
		{name: "over limit rejected", size: messageReadersBodyLimit + 1, code: errs.ErrArgs.Code()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &Api{imApiCaller: &messageReadersHandlerCaller{}}
			body := paddedMessageReadersBody(tc.size)
			if len(body) != tc.size {
				t.Fatalf("body size = %d, want %d", len(body), tc.size)
			}
			response := serveMessageReadersHandler(t, api, body, true)
			if response.ErrCode != tc.code {
				t.Fatalf("body size %d errCode = %d, want %d", tc.size, response.ErrCode, tc.code)
			}
		})
	}
}

func TestMessageReadersHandlerSeparatesPermissionFromUpstreamFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cause    error
		wantCode int
	}{
		{
			name:     "permission denied",
			cause:    rtcauthority.ErrPermission,
			wantCode: errs.ErrNoPermission.Code(),
		},
		{
			name:     "upstream unavailable",
			cause:    errors.New("upstream private error detail"),
			wantCode: errs.ErrInternalServer.Code(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &Api{extrasIMURL: "http://unused.invalid", imApiCaller: &messageReadersHandlerCaller{tokenErr: tc.cause}}
			response := serveMessageReadersHandler(t, api, `{"conversationID":"sg_test","messageID":"message","seq":5}`, true)
			if response.ErrCode != tc.wantCode {
				t.Fatalf("handler errCode = %d, want %d (response %+v)", response.ErrCode, tc.wantCode, response)
			}
			if tc.name == "upstream unavailable" && strings.Contains(response.ErrDlt, "upstream private error detail") {
				t.Fatalf("upstream details leaked through error response: %+v", response)
			}
		})
	}
}

func TestGroupReadersRejectNonmemberBeforeMissingConversation(t *testing.T) {
	var conversationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/group/get_group_members_info" {
			_, _ = w.Write([]byte(`{"errCode":0,"data":{"members":[]}}`))
			return
		}
		conversationCalls.Add(1)
		_, _ = w.Write([]byte(`{"errCode":500,"errMsg":"conversation missing"}`))
	}))
	defer server.Close()
	api := Api{extrasIMURL: server.URL, imApiCaller: extrasGroupCheckCaller{groups: []*sdkws.GroupInfo{{GroupID: "group"}}}}
	_, err := api.groupMessageReaders(context.Background(), "outsider", messageReadersRequest{ConversationID: "sg_group", MessageID: "message", Seq: 5})
	if !errors.Is(err, rtcauthority.ErrPermission) || conversationCalls.Load() != 0 {
		t.Fatalf("nonmember must be denied before conversation lookup: err=%v calls=%d", err, conversationCalls.Load())
	}
}
