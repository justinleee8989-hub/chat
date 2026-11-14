package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	chatconstant "github.com/openimsdk/chat/pkg/common/constant"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/chat/pkg/common/messageextras"
	"github.com/openimsdk/protocol/sdkws"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHistoryPaginationAllAvailableSequences(t *testing.T) {
	for _, tc := range []struct {
		name                                                                string
		outsider, broken, missing, sparse, incomplete, joinedLater, deleted bool
	}{{name: "member"}, {name: "join visibility", joinedLater: true}, {name: "deleted and revoked", deleted: true}, {name: "partial upstream page", incomplete: true}, {name: "sparse full history", sparse: true}, {name: "outsider", outsider: true}, {name: "old member", outsider: true}, {name: "upstream", broken: true}, {name: "swallowed upstream", missing: true}} {
		t.Run(tc.name, func(t *testing.T) {
			pulls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]json.RawMessage
				_ = json.NewDecoder(r.Body).Decode(&req)
				var data any
				switch r.URL.Path {
				case "/conversation/get_conversation":
					data = map[string]any{"conversation": map[string]any{"ownerUserID": "alice", "conversationID": "sg_g", "groupID": "g"}}
				case "/group/get_group_members_info":
					members := []any{}
					if !tc.outsider {
						members = append(members, map[string]any{"userID": "alice"})
					}
					data = map[string]any{"members": members}
				case "/msg/get_conversations_has_read_and_max_seq":
					data = map[string]any{"seqs": map[string]any{"sg_g": map[string]any{"maxSeq": 1205}}}
				case "/msg/pull_msg_by_seq":
					pulls++
					if tc.broken {
						fmt.Fprint(w, `{"errCode":500,"errMsg":"offline"}`)
						return
					}
					if tc.missing {
						data = map[string]any{"msgs": map[string]any{}}
						break
					}
					var user string
					_ = json.Unmarshal(req["userID"], &user)
					if user != "alice" {
						t.Errorf("pull actor %q", user)
					}
					var ranges []*sdkws.SeqRange
					_ = json.Unmarshal(req["seqRanges"], &ranges)
					span := ranges[0]
					if span.End-span.Begin+1 > 500 || span.Num != span.End-span.Begin+1 {
						t.Error("unbounded pull")
					}
					rows := []*sdkws.MsgData{}
					for seq := span.Begin; seq <= span.End; seq++ {
						if tc.joinedLater && seq < 1100 {
							continue
						}
						if tc.incomplete && seq == span.End-1 {
							continue
						}
						content := `{"content":"needle"}`
						if tc.sparse && seq != 1 {
							content = `{"content":"other"}`
						}
						rows = append(rows, &sdkws.MsgData{Seq: seq, ClientMsgID: fmt.Sprint(seq), SendID: "alice", GroupID: "g", ContentType: 101, Content: []byte(content)})
						if tc.deleted && seq%2 == 0 {
							rows[len(rows)-1].Status = 4
						}
						if tc.deleted && seq%2 == 1 && seq != 1 {
							rows[len(rows)-1].ContentType = 2101
						}
					}
					data = map[string]any{"msgs": map[string]any{"sg_g": map[string]any{"Msgs": rows, "isEnd": span.Begin == 1 || (tc.joinedLater && span.Begin <= 1100)}}}
				default:
					t.Errorf("unexpected API %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"errCode": 0, "data": data})
			}))
			defer server.Close()
			api := Api{extrasIMURL: server.URL, imApiCaller: extrasGroupCheckCaller{groups: []*sdkws.GroupInfo{{GroupID: "g"}}}}
			req := messageHistoryRequest{ConversationID: "sg_g", Kind: "text", Query: "needle", Limit: 73}
			seen := map[int64]bool{}
			last := int64(1206)
			for page := 0; page < 30; page++ {
				got, err := api.messageHistory(context.Background(), "alice", req)
				if tc.outsider || tc.broken || tc.missing || tc.incomplete {
					if err == nil {
						t.Fatal("expected failure")
					}
					if tc.outsider && pulls != 0 {
						t.Fatal("outsider reached history")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range got.Messages {
					seq := int64(m["seq"].(float64))
					if seen[seq] || seq >= last {
						t.Fatal("duplicate or unsorted")
					}
					seen[seq] = true
					last = seq
				}
				if !got.HasMore {
					break
				}
				if got.Cursor == "" || got.Cursor == req.Cursor {
					t.Fatal("cursor not progressing")
				}
				req.Cursor = got.Cursor
			}
			want := 1205
			if tc.joinedLater {
				want = 106
			}
			if tc.deleted {
				want = 1
			}
			if tc.sparse {
				want = 1
				if pulls != 3 {
					t.Fatalf("expected complete sparse scan, calls %d", pulls)
				}
			}
			if len(seen) != want {
				t.Fatalf("silent cutoff: %d", len(seen))
			}
		})
	}
}

func TestHistoryProjectionMatchesEditedTextAndLinks(t *testing.T) {
	msg := &sdkws.MsgData{Seq: 1, ClientMsgID: "m", ContentType: 101, Content: []byte(`{"content":"old"}`)}
	edited := "fresh https://example.com"
	got, ok := projectHistoryMessage(msg, &messageextras.Record{Text: &edited, EditedAt: 123}, messageHistoryRequest{Kind: "links", Query: "fresh"})
	if !ok || got["textElem"].(map[string]any)["content"] != edited {
		t.Fatalf("edited message not matched: %+v", got)
	}
	if _, ok = projectHistoryMessage(msg, &messageextras.Record{Text: &edited}, messageHistoryRequest{Kind: "text", Query: "old"}); ok {
		t.Fatal("stale original text matched")
	}
	for _, m := range []*sdkws.MsgData{{Status: 4, ContentType: 101}, {ContentType: 2101}, {ClientMsgID: "bad", ContentType: 101, Content: []byte("malformed")}} {
		if _, ok := projectHistoryMessage(m, nil, messageHistoryRequest{Kind: "all"}); ok {
			t.Fatal("invalid/deleted/revoked matched")
		}
	}
	if string(msg.Content) != `{"content":"old"}` {
		t.Fatal("mutated original")
	}
}

func TestHistoryHandlerRejectsBadInputBeforeUpstream(t *testing.T) {
	for _, body := range []string{`{`, `{"conversationID":"sg_g","padding":"` + strings.Repeat("x", 8192) + `"}`, `{"conversationID":"sg_g","limit":101}`, `{"conversationID":"sg_g","limit":-1}`, `{"conversationID":"sg_g","kind":"admin"}`, `{"conversationID":"sg_g","query":"` + strings.Repeat("x", 257) + `"}`, `{"conversationID":"sg_g","cursor":"bad"}`} {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.ContextWithFallback = true
		caller := &messageReadersHandlerCaller{}
		api := Api{extrasIMURL: "http://unused.invalid", imApiCaller: caller}
		r.POST("/history", api.MessageHistory)
		req := httptest.NewRequest("POST", "/history", strings.NewReader(body))
		req = req.WithContext(mctx.WithOpUserID(req.Context(), "alice", chatconstant.NormalUser))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var response struct {
			ErrCode int `json:"errCode"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &response)
		if response.ErrCode == 0 || caller.tokenCalls.Load() != 0 {
			t.Fatalf("invalid input reached upstream: %s %s", body, w.Body.String())
		}
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := Api{}
	r.POST("/history", api.MessageHistory)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/history", strings.NewReader(`{"conversationID":"sg_g"}`)))
	if strings.Contains(w.Body.String(), `"errCode":0`) {
		t.Fatal("anonymous accepted")
	}
}

func TestHistoryCursorBoundToSearchAndActor(t *testing.T) {
	c := historyCursor{Actor: "alice", Conversation: "sg_g", Kind: "text", Query: "query", Snapshot: 100, Next: 80}
	raw, _ := json.Marshal(c)
	cursor := base64.RawURLEncoding.EncodeToString(raw)
	for _, tc := range []struct {
		actor, conversation, kind, query string
		valid                            bool
	}{
		{"alice", "sg_g", "text", "query", true}, {"bob", "sg_g", "text", "query", false}, {"alice", "sg_other", "text", "query", false}, {"alice", "sg_g", "links", "query", false}, {"alice", "sg_g", "text", "changed", false},
	} {
		req := messageHistoryRequest{ConversationID: tc.conversation, Kind: tc.kind, Query: tc.query, Cursor: cursor}
		_, err := validateHistoryRequest(tc.actor, &req)
		if (err == nil) != tc.valid {
			t.Fatalf("cursor scope validation %+v %v", tc, err)
		}
	}
}
func TestHistoryProjectionPreservesMetadataAndMedia(t *testing.T) {
	for _, tc := range []struct {
		kind, field, content string
		typ                  int32
	}{{"images", "pictureElem", `{"sourcePicture":{"url":"https://example.com/a.png"}}`, 102}, {"files", "fileElem", `{"fileName":"report.pdf","sourceUrl":"https://example.com/report.pdf"}`, 105}} {
		m := &sdkws.MsgData{ClientMsgID: "id", ContentType: tc.typ, Content: []byte(tc.content), Ex: `{"replyTo":"reply"}`}
		got, ok := projectHistoryMessage(m, nil, messageHistoryRequest{Kind: tc.kind})
		if !ok || got[tc.field] == nil || got["ex"] != m.Ex {
			t.Fatalf("media or rich metadata lost %+v", got)
		}
	}
}

func TestHistorySDKNormalizationAndRichProjection(t *testing.T) {
	edited := "edited text"
	record := &messageextras.Record{Text: &edited, EditedAt: 22, Revision: 3}
	for _, tc := range []struct {
		typ                         int32
		kind, content, query, field string
	}{
		{101, "text", `{"content":"original"}`, "edited", "textElem"},
		{106, "text", `{"text":"original"}`, "edited", "atTextElem"},
		{114, "text", `{"text":"original"}`, "edited", "quoteElem"},
		{108, "all", `{"nickname":"Nick","userID":"alice"}`, "nick", "cardElem"},
		{109, "all", `{"description":"Shanghai","latitude":31,"longitude":121}`, "Shanghai", "locationElem"},
	} {
		m := &sdkws.MsgData{ClientMsgID: "m", ContentType: tc.typ, Content: []byte(tc.content), SendTime: 1700000000123, CreateTime: 1700000000000, Ex: `{"imRichReplyV1":{}}`}
		got, ok := projectHistoryMessage(m, record, messageHistoryRequest{Kind: tc.kind, Query: tc.query})
		if !ok {
			t.Fatalf("rich type %d not matched", tc.typ)
		}
		if got["status"] != 2 || got[tc.field] == nil || got["projection"] == nil || got["sendTime"] != float64(m.SendTime) || got["ex"] != m.Ex {
			t.Fatalf("incorrect normalization: %+v", got)
		}
	}
}

func TestHistoryGroupMembershipPrecedesConversationLookup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		broken bool
		want   int
	}{{"outsider without conversation", false, 1002}, {"membership upstream failure", true, 500}} {
		t.Run(tc.name, func(t *testing.T) {
			lookups := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/group/get_group_members_info" {
					if tc.broken {
						fmt.Fprint(w, `{"errCode":500,"errMsg":"offline"}`)
					} else {
						fmt.Fprint(w, `{"errCode":0,"data":{"members":[]}}`)
					}
					return
				}
				lookups++
				fmt.Fprint(w, `{"errCode":500,"errMsg":"conversation missing"}`)
			}))
			defer upstream.Close()
			api := Api{extrasIMURL: upstream.URL, imApiCaller: extrasGroupCheckCaller{groups: []*sdkws.GroupInfo{{GroupID: "g"}}}}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.ContextWithFallback = true
			router.POST("/history", api.MessageHistory)
			req := httptest.NewRequest("POST", "/history", strings.NewReader(`{"conversationID":"sg_g"}`))
			req = req.WithContext(mctx.WithOpUserID(req.Context(), "outsider", chatconstant.NormalUser))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			var response struct {
				ErrCode int `json:"errCode"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &response)
			if response.ErrCode != tc.want || lookups != 0 {
				t.Fatalf("errCode=%d want=%d conversation lookups=%d", response.ErrCode, tc.want, lookups)
			}
		})
	}
}
