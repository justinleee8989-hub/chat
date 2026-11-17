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
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	chatconstant "github.com/openimsdk/chat/pkg/common/constant"
	"github.com/openimsdk/chat/pkg/common/messageextras"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/redis/go-redis/v9"
)

func TestVisibleExtraRecordsRequiresActorVisibleMatchingMessage(t *testing.T) {
	cases := []struct {
		name        string
		id          string
		seq         int64
		status      int32
		contentType int32
		want        bool
	}{
		{name: "visible", id: "target", seq: 3, contentType: 101, want: true},
		{name: "deleted", id: "target", seq: 3, status: 4},
		{name: "revoked", id: "target", seq: 3, contentType: 1001},
		{name: "mismatched id", id: "other", seq: 3, contentType: 101},
		{name: "mismatched sequence", id: "target", seq: 4, contentType: 101},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pulls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/msg/get_conversations_has_read_and_max_seq":
					_, _ = fmt.Fprint(w, `{"errCode":0,"data":{"seqs":{"sg_test":{"maxSeq":10}}}}`)
				case "/msg/pull_msg_by_seq":
					pulls++
					var req sdkws.PullMessageBySeqsReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if req.UserID != "alice" {
						t.Errorf("pull user=%q", req.UserID)
					}
					row := &sdkws.MsgData{Seq: tc.seq, ClientMsgID: tc.id, ContentType: tc.contentType, Status: tc.status}
					resp := &sdkws.PullMessageBySeqsResp{Msgs: map[string]*sdkws.PullMsgs{"sg_test": {Msgs: []*sdkws.MsgData{row}}}}
					_ = json.NewEncoder(w).Encode(map[string]any{"errCode": 0, "data": resp})
				default:
					t.Errorf("unexpected endpoint %q", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			api := &Api{extrasIMURL: server.URL, imApiCaller: testMessageExtrasCaller{}}
			records := map[string]*messageextras.Record{"target": {Seq: 3, Text: ptr("metadata")}}
			got, err := api.visibleExtraRecords(context.Background(), "alice", "sg_test", 1, 10, records)
			if err != nil {
				t.Fatal(err)
			}
			if (got["target"] != nil) != tc.want {
				t.Fatalf("target visible=%t, want %t; records=%v", got["target"] != nil, tc.want, got)
			}
			if pulls != 1 {
				t.Fatalf("pulls=%d, want 1", pulls)
			}
		})
	}
}

func TestVisibleExtraRecordsRejectsOutOfBoundsSequence(t *testing.T) {
	pulls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/msg/get_conversations_has_read_and_max_seq" {
			_, _ = fmt.Fprint(w, `{"errCode":0,"data":{"seqs":{"sg_test":{"maxSeq":10}}}}`)
			return
		}
		if r.URL.Path == "/msg/pull_msg_by_seq" {
			pulls++
		}
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	api := &Api{extrasIMURL: server.URL, imApiCaller: testMessageExtrasCaller{}}
	got, err := api.visibleExtraRecords(context.Background(), "alice", "sg_test", 1, 10, map[string]*messageextras.Record{"target": {Seq: 11}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || pulls != 0 {
		t.Fatalf("got=%v pulls=%d, want no visible records and no pull", got, pulls)
	}
}

func TestVisibleExtraRecordsRejectsMissingVisibilityBounds(t *testing.T) {
	pulls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"errCode":0,"data":{"seqs":{}}}`)
		if r.URL.Path == "/msg/pull_msg_by_seq" {
			pulls++
		}
	}))
	defer server.Close()
	api := &Api{extrasIMURL: server.URL, imApiCaller: testMessageExtrasCaller{}}
	got, err := api.visibleExtraRecords(context.Background(), "alice", "sg_test", 1, 10, map[string]*messageextras.Record{"target": {Seq: 3}})
	if err == nil {
		t.Fatal("expected missing visibility bounds to fail")
	}
	if got != nil || pulls != 0 {
		t.Fatalf("got=%v pulls=%d, expected failure without a pull", got, pulls)
	}
}

func TestVisibleExtraRecordsCachedHintStillRequiresActorVisiblePull(t *testing.T) {
	pulls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/msg/get_conversations_has_read_and_max_seq":
			_, _ = fmt.Fprint(w, `{"errCode":0,"data":{"seqs":{"sg_cached":{"maxSeq":10}}}}`)
		case "/msg/pull_msg_by_seq":
			pulls++
			row := &sdkws.MsgData{Seq: 3, ClientMsgID: "target", Status: 4, ContentType: 101}
			resp := &sdkws.PullMessageBySeqsResp{Msgs: map[string]*sdkws.PullMsgs{"sg_cached": {Msgs: []*sdkws.MsgData{row}}}}
			_ = json.NewEncoder(w).Encode(map[string]any{"errCode": 0, "data": resp})
		default:
			t.Errorf("unexpected endpoint %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	extraSequenceHints.put(server.URL, "sg_cached", "target", 3)
	api := &Api{extrasIMURL: server.URL, imApiCaller: testMessageExtrasCaller{}}
	got, err := api.visibleExtraRecords(context.Background(), "alice", "sg_cached", 1, 10, map[string]*messageextras.Record{"target": {Text: ptr("stale")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || pulls != 1 {
		t.Fatalf("cached sequence hint authorized metadata: got=%v pulls=%d", got, pulls)
	}
}

func TestVisibleExtraRecordsResolvesLegacySequenceWithoutMutatingInput(t *testing.T) {
	var requestedUser string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/msg/get_conversations_has_read_and_max_seq":
			_, _ = fmt.Fprint(w, `{"errCode":0,"data":{"seqs":{"sg_test":{"maxSeq":1201}}}}`)
		case "/msg/pull_msg_by_seq":
			var req sdkws.PullMessageBySeqsReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			requestedUser = req.UserID
			if len(req.SeqRanges) != 1 || req.SeqRanges[0].End-req.SeqRanges[0].Begin+1 > 500 {
				t.Errorf("unbounded legacy scan: %+v", req.SeqRanges)
			}
			span := req.SeqRanges[0]
			rows := []*sdkws.MsgData{}
			for seq := span.End; seq >= span.Begin; seq-- {
				id := fmt.Sprintf("seq-%d", seq)
				if seq == 801 {
					id = "target"
				}
				rows = append(rows, &sdkws.MsgData{Seq: seq, ClientMsgID: id, ContentType: 101})
			}
			resp := &sdkws.PullMessageBySeqsResp{Msgs: map[string]*sdkws.PullMsgs{"sg_test": {Msgs: rows, IsEnd: span.Begin == 1}}}
			_ = json.NewEncoder(w).Encode(map[string]any{"errCode": 0, "data": resp})
		default:
			t.Errorf("unexpected endpoint %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api := &Api{extrasIMURL: server.URL, imApiCaller: testMessageExtrasCaller{}}
	original := &messageextras.Record{Text: ptr("legacy")}
	got, err := api.visibleExtraRecords(context.Background(), "alice", "sg_test", 1, 1201, map[string]*messageextras.Record{"target": original})
	if err != nil {
		t.Fatal(err)
	}
	if requestedUser != "alice" {
		t.Fatalf("legacy pull user=%q", requestedUser)
	}
	if got["target"] == nil || got["target"].Seq != 801 {
		t.Fatalf("resolved record=%+v", got["target"])
	}
	if original.Seq != 0 {
		t.Fatalf("input record mutated: %+v", original)
	}
}

func TestVisibleExtraRecordsResumesBoundedLegacyScan(t *testing.T) {
	const maxSeq = int64(5001)
	pulls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/msg/get_conversations_has_read_and_max_seq":
			_, _ = fmt.Fprintf(w, `{"errCode":0,"data":{"seqs":{"sg_long":{"maxSeq":%d}}}}`, maxSeq)
		case "/msg/pull_msg_by_seq":
			pulls++
			var req sdkws.PullMessageBySeqsReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req.UserID != "alice" {
				t.Errorf("pull user=%q", req.UserID)
			}
			if len(req.SeqRanges) != 1 || req.SeqRanges[0].End-req.SeqRanges[0].Begin+1 > 500 {
				t.Errorf("unbounded scan range: %+v", req.SeqRanges)
			}
			span := req.SeqRanges[0]
			rows := make([]*sdkws.MsgData, 0, span.End-span.Begin+1)
			for seq := span.End; seq >= span.Begin; seq-- {
				id := fmt.Sprintf("seq-%d", seq)
				if seq == 501 {
					id = "target"
				}
				rows = append(rows, &sdkws.MsgData{Seq: seq, ClientMsgID: id, ContentType: 101})
			}
			resp := &sdkws.PullMessageBySeqsResp{Msgs: map[string]*sdkws.PullMsgs{"sg_long": {Msgs: rows, IsEnd: span.Begin == 1}}}
			_ = json.NewEncoder(w).Encode(map[string]any{"errCode": 0, "data": resp})
		default:
			t.Errorf("unexpected endpoint %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	extraSequenceHints.put(server.URL, "sg_long", "target", 0)
	key := legacyScanKey{server: server.URL, actor: "alice", conversation: "sg_long", message: "target", lower: 1}
	defer func() {
		extraLegacyScans.mu.Lock()
		delete(extraLegacyScans.items, key)
		extraLegacyScans.mu.Unlock()
	}()
	api := &Api{extrasIMURL: server.URL, imApiCaller: testMessageExtrasCaller{}}
	original := &messageextras.Record{Text: ptr("old")}
	input := map[string]*messageextras.Record{"target": original}
	if got, err := api.visibleExtraRecords(context.Background(), "alice", "sg_long", 1, maxSeq, input); err == nil || got != nil {
		t.Fatalf("first bounded scan should ask for retry without partial result: got=%v err=%v", got, err)
	}
	if pulls != 8 {
		t.Fatalf("first request pulls=%d, want 8", pulls)
	}
	got, err := api.visibleExtraRecords(context.Background(), "alice", "sg_long", 1, maxSeq, input)
	if err != nil {
		t.Fatal(err)
	}
	if got["target"] == nil || got["target"].Seq != 501 {
		t.Fatalf("resumed record=%+v", got["target"])
	}
	if original.Seq != 0 {
		t.Fatalf("input record mutated: %+v", original)
	}
	if pulls != 10 {
		t.Fatalf("total pulls=%d, want 10 after resuming", pulls)
	}
}

func TestVisibleExtraRecordsUpstreamFailureReturnsNoPartialRecords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/msg/get_conversations_has_read_and_max_seq" {
			_, _ = fmt.Fprint(w, `{"errCode":0,"data":{"seqs":{"sg_test":{"maxSeq":10}}}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"errCode":500,"errMsg":"offline"}`)
	}))
	defer server.Close()
	api := &Api{extrasIMURL: server.URL, imApiCaller: testMessageExtrasCaller{}}
	got, err := api.visibleExtraRecords(context.Background(), "alice", "sg_test", 1, 10, map[string]*messageextras.Record{"target": {Seq: 3}})
	if err == nil {
		t.Fatal("expected upstream pull failure")
	}
	if got != nil {
		t.Fatalf("partial records returned on failure: %v", got)
	}
}

func TestMessageExtrasGetRechecksCachedRecordAgainstActorVisibility(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int32
		contentType int32
		want        bool
	}{{name: "visible", contentType: 101, want: true}, {name: "old revoked", contentType: 1001}} {
		t.Run(tc.name, func(t *testing.T) {
			redisServer, addr := startVisibilityRedis(t, `{"seq":3,"text":"cached edit"}`)
			defer redisServer.Close()
			redisClient := redis.NewClient(&redis.Options{Addr: addr, Protocol: 2})
			defer redisClient.Close()
			var pulls int
			imServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/conversation/get_conversation":
					_, _ = fmt.Fprint(w, `{"errCode":0,"data":{"conversation":{"ownerUserID":"alice","conversationID":"sg_cached","minSeq":1,"maxSeq":10}}}`)
				case "/msg/get_conversations_has_read_and_max_seq":
					_, _ = fmt.Fprint(w, `{"errCode":0,"data":{"seqs":{"sg_cached":{"maxSeq":10}}}}`)
				case "/msg/pull_msg_by_seq":
					pulls++
					var req sdkws.PullMessageBySeqsReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if req.UserID != "alice" {
						t.Errorf("pull actor=%q", req.UserID)
					}
					row := &sdkws.MsgData{Seq: 3, ClientMsgID: "target", ContentType: tc.contentType, Status: tc.status}
					resp := &sdkws.PullMessageBySeqsResp{Msgs: map[string]*sdkws.PullMsgs{"sg_cached": {Msgs: []*sdkws.MsgData{row}}}}
					_ = json.NewEncoder(w).Encode(map[string]any{"errCode": 0, "data": resp})
				default:
					t.Errorf("unexpected endpoint %q", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer imServer.Close()
			api := &Api{extrasRedis: redisClient, extrasIMURL: imServer.URL, imApiCaller: testMessageExtrasCaller{}}
			got, err := api.messageExtras(context.Background(), "alice", messageextras.Request{Action: "get", ConversationID: "sg_cached", MessageIDs: []string{"target"}})
			if err != nil {
				t.Fatal(err)
			}
			if (got["target"] != nil) != tc.want {
				t.Fatalf("metadata visible=%t want=%t, got=%v", got["target"] != nil, tc.want, got)
			}
			if pulls != 1 {
				t.Fatalf("actor visibility pulls=%d, want 1", pulls)
			}
		})
	}
}

func TestMessageExtrasRequestCancellationWorksWithoutGinFallback(t *testing.T) {
	redisServer, addr := startVisibilityRedis(t, `{"seq":3,"text":"cached edit"}`)
	defer redisServer.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: addr, Protocol: 2})
	defer redisClient.Close()
	started := make(chan struct{})
	canceled := make(chan struct{})
	imServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/conversation/get_conversation" {
			_, _ = io.Copy(io.Discard, r.Body)
			close(started)
			<-r.Context().Done()
			close(canceled)
			return
		}
		t.Errorf("unexpected endpoint reached after cancellation: %q", r.URL.Path)
	}))
	defer func() {
		imServer.CloseClientConnections()
		imServer.Close()
	}()
	api := &Api{extrasRedis: redisClient, extrasIMURL: imServer.URL, imApiCaller: testMessageExtrasCaller{}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// Keep Gin's fallback disabled to prove handlers pass Request.Context.
	router.Use(func(c *gin.Context) {
		// Authentication middleware stores these keys on Gin's context.
		c.Set(chatconstant.RpcOpUserID, "alice")
		c.Set(chatconstant.RpcOpUserType, []string{strconv.Itoa(chatconstant.NormalUser)})
		c.Next()
	})
	router.POST("/extras", api.MessageExtras)
	requestCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/extras", strings.NewReader(`{"action":"get","conversationID":"sg_cached","messageIDs":["target"]}`)).WithContext(requestCtx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { router.ServeHTTP(w, req); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("request cancellation did not reach upstream with Gin fallback disabled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after request cancellation")
	}
}

func startVisibilityRedis(t *testing.T, metadata string) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				reader, writer := bufio.NewReader(conn), bufio.NewWriter(conn)
				for {
					args, err := readVisibilityRESP(reader)
					if err != nil {
						return
					}
					switch strings.ToUpper(args[0]) {
					case "CLIENT":
						_, _ = writer.WriteString("+OK\r\n")
					case "HMGET":
						mu.Lock()
						value := metadata
						mu.Unlock()
						_, _ = fmt.Fprintf(writer, "*%d\r\n", len(args)-2)
						for range args[2:] {
							_, _ = fmt.Fprintf(writer, "$%d\r\n%s\r\n", len(value), value)
						}
					default:
						_, _ = fmt.Fprintf(writer, "-ERR unsupported %s\r\n", args[0])
					}
					if err := writer.Flush(); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener, listener.Addr().String()
}

func readVisibilityRESP(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) == 0 || line[0] != '*' {
		return nil, fmt.Errorf("expected array request, got %q", line)
	}
	count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, count)
	for i := range args {
		line, err = reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if len(line) == 0 || line[0] != '$' {
			return nil, fmt.Errorf("expected bulk request, got %q", line)
		}
		length, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil {
			return nil, err
		}
		data := make([]byte, length+2)
		if _, err = io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		args[i] = string(data[:length])
	}
	return args, nil
}

func ptr(s string) *string { return &s }
