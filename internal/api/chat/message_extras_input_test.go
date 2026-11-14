package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/openimsdk/chat/pkg/common/constant"
	"github.com/openimsdk/chat/pkg/common/imapi"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/chat/pkg/common/messageextras"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/redis/go-redis/v9"
)

func TestMessageExtrasEnableFlagIsIndependentAndBackwardCompatible(t *testing.T) {
	for _, tc := range []struct {
		name, configured string
		rtcEnabled, want bool
	}{
		{"legacy rtc enabled", "", true, true},
		{"legacy rtc disabled", "", false, false},
		{"explicitly enabled without rtc", "true", false, true},
		{"explicitly disabled with rtc", "false", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageExtrasEnabled(tc.configured, tc.rtcEnabled); got != tc.want {
				t.Fatalf("messageExtrasEnabled(%q, %v) = %v, want %v", tc.configured, tc.rtcEnabled, got, tc.want)
			}
		})
	}
}

func TestInitRTCOffCanInitializeMessageExtrasIndependently(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	stop := make(chan struct{})
	defer close(stop)
	go serveTestRedis(listener, stop)
	t.Setenv("RTC_ENABLED", "false")
	t.Setenv("MESSAGE_EXTRAS_ENABLED", "true")
	t.Setenv("MESSAGE_EXTRAS_REDIS_ADDR", listener.Addr().String())
	t.Setenv("MESSAGE_EXTRAS_REDIS_PASSWORD", "")
	t.Setenv("RTC_REDIS_ADDR", "")
	t.Setenv("RTC_REDIS_PASSWORD", "")
	api := &Api{imApiCaller: testMessageExtrasCaller{}}
	if err := api.initRTC(context.Background(), "http://im.test"); err != nil {
		t.Fatalf("init with RTC off and extras on: %v", err)
	}
	if api.rtcAuthority != nil {
		t.Fatal("RTC authority initialized despite RTC_ENABLED=false")
	}
	if api.extrasRedis == nil || api.extrasIMURL != "http://im.test" {
		t.Fatal("message extras was not independently initialized")
	}
	_ = api.extrasRedis.Close()
}

func serveTestRedis(listener net.Listener, stop <-chan struct{}) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			reader := bufio.NewReader(conn)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if len(line) == 0 || line[0] != '*' {
					continue
				}
				count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
				if err != nil {
					return
				}
				args := make([]string, count)
				for i := range args {
					lengthLine, e := reader.ReadString('\n')
					if e != nil {
						return
					}
					length, e := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(lengthLine, "$")))
					if e != nil {
						return
					}
					buffer := make([]byte, length+2)
					if _, e = io.ReadFull(reader, buffer); e != nil {
						return
					}
					args[i] = string(buffer[:length])
				}
				if len(args) > 0 && strings.EqualFold(args[0], "HELLO") {
					_, _ = conn.Write([]byte("%7\r\n+server\r\n+redis\r\n+version\r\n+7.0.0\r\n+proto\r\n:3\r\n+id\r\n:1\r\n+mode\r\n+standalone\r\n+role\r\n+master\r\n+modules\r\n*0\r\n"))
				} else if len(args) > 0 && strings.EqualFold(args[0], "PING") {
					_, _ = conn.Write([]byte("+PONG\r\n"))
				} else {
					_, _ = conn.Write([]byte("+OK\r\n"))
				}
			}
		}(conn)
		select {
		case <-stop:
			return
		default:
		}
	}
}

type groupCheckCaller struct {
	testMessageExtrasCaller
	groups []*sdkws.GroupInfo
}

func (c groupCheckCaller) FindGroupInfo(context.Context, []string) ([]*sdkws.GroupInfo, error) {
	return c.groups, nil
}

func TestMessageExtrasGroupCheckWorksWithoutRTCServiceAndRequiresMembership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, response string
		wantErr        bool
	}{
		{"current member", `{"errCode":0,"data":{"members":[{"userID":"qa-user"}]}}`, false},
		{"outsider", `{"errCode":0,"data":{"members":[]}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()
			caller := groupCheckCaller{groups: []*sdkws.GroupInfo{{GroupID: "g1"}}}
			err := checkMessageExtrasGroupMember(context.Background(), server.URL, caller, "g1", "qa-user")
			if (err != nil) != tc.wantErr {
				t.Fatalf("membership error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

type extrasGroupCheckCaller struct {
	testMessageExtrasCaller
	groups []*sdkws.GroupInfo
}

func (c extrasGroupCheckCaller) FindGroupInfo(context.Context, []string) ([]*sdkws.GroupInfo, error) {
	return c.groups, nil
}

func TestMessageExtrasGroupReadWorksWithRTCDisabledButRequiresMember(t *testing.T) {
	for _, tc := range []struct {
		name, member string
		wantErr      bool
	}{
		{"member", "qa-user", false}, {"outsider", "another-user", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/conversation/get_conversation":
					_, _ = w.Write([]byte(`{"errCode":0,"data":{"conversation":{"ownerUserID":"qa-user","groupID":"g1"}}}`))
				case "/group/get_group_members_info":
					_, _ = w.Write([]byte(fmt.Sprintf(`{"errCode":0,"data":{"members":[{"userID":%q}]}}`, tc.member)))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
			defer redisClient.Close()
			api := &Api{extrasRedis: redisClient, extrasIMURL: server.URL, imApiCaller: extrasGroupCheckCaller{groups: []*sdkws.GroupInfo{{GroupID: "g1"}}}}
			_, err := api.messageExtras(context.Background(), "qa-user", messageextras.Request{Action: "get", ConversationID: "sg_test"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("messageExtras error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

const testMessageExtrasBodyLimit = 128 << 10

type testMessageExtrasCaller struct{ imapi.CallerInterface }

func (testMessageExtrasCaller) ImAdminTokenWithDefaultAdmin(context.Context) (string, error) {
	return "test-admin-token", nil
}

func messageExtrasRequest(t *testing.T, body string) int32 {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var conversationCalls atomic.Int32
	imServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/get_conversation":
			conversationCalls.Add(1)
			_, _ = w.Write([]byte(`{"errCode":0,"data":{"conversation":{"ownerUserID":"qa-user","groupID":""}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer imServer.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer redisClient.Close()
	r := gin.New()
	r.ContextWithFallback = true
	api := &Api{
		extrasRedis: redisClient,
		extrasIMURL: imServer.URL,
		imApiCaller: testMessageExtrasCaller{},
	}
	r.POST("/user/message/extras", api.MessageExtras)
	req := httptest.NewRequest(http.MethodPost, "/user/message/extras", strings.NewReader(body))
	req = req.WithContext(mctx.WithOpUserID(req.Context(), "qa-user", constant.NormalUser))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return conversationCalls.Load()
}

func TestMessageExtrasRejectsBodyOver128KiB(t *testing.T) {
	prefix := `{"action":"get","conversationID":"sg_test","padding":"`
	suffix := `"}`
	padding := strings.Repeat("x", testMessageExtrasBodyLimit-len(prefix)-len(suffix)+1)
	if got := messageExtrasRequest(t, prefix+padding+suffix); got != 0 {
		t.Fatalf("oversized request reached OpenIM conversation lookup %d times", got)
	}
}

func TestMessageExtrasAllowsBodyAt128KiB(t *testing.T) {
	prefix := `{"action":"get","conversationID":"sg_test","padding":"`
	suffix := `"}`
	padding := strings.Repeat("x", testMessageExtrasBodyLimit-len(prefix)-len(suffix))
	body := prefix + padding + suffix
	if len(body) != testMessageExtrasBodyLimit {
		t.Fatalf("test body size = %d, want %d", len(body), testMessageExtrasBodyLimit)
	}
	if got := messageExtrasRequest(t, body); got != 1 {
		t.Fatalf("valid body at limit reached OpenIM conversation lookup %d times, want 1", got)
	}
}

func TestMessageExtrasRejectsAnyMessageIDLongerThan128Bytes(t *testing.T) {
	id := strings.Repeat("x", 129)
	body := fmt.Sprintf(`{"action":"get","conversationID":"sg_test","messageIDs":[%q]}`, id)
	if got := messageExtrasRequest(t, body); got != 0 {
		t.Fatalf("overlong message ID reached OpenIM conversation lookup %d times", got)
	}
}

func TestMessageExtrasRejectsMoreThan100MessageIDs(t *testing.T) {
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
	}
	encoded, err := json.Marshal(map[string]any{"action": "get", "conversationID": "sg_test", "messageIDs": ids})
	if err != nil {
		t.Fatal(err)
	}
	if got := messageExtrasRequest(t, string(encoded)); got != 0 {
		t.Fatalf("request with too many message IDs reached OpenIM conversation lookup %d times", got)
	}
}

func TestMessageExtrasAccepts100IDsOf128Bytes(t *testing.T) {
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = strings.Repeat("x", 128)
	}
	encoded, err := json.Marshal(map[string]any{"action": "get", "conversationID": "sg_test", "messageIDs": ids})
	if err != nil {
		t.Fatal(err)
	}
	if got := messageExtrasRequest(t, string(encoded)); got != 1 {
		t.Fatalf("maximum valid ID list reached OpenIM conversation lookup %d times, want 1", got)
	}
}
