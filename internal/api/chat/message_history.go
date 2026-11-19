package chat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/openimsdk/chat/pkg/common/imapi"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/chat/pkg/common/messageextras"
	"github.com/openimsdk/chat/pkg/common/rtcauthority"
	"github.com/openimsdk/chat/pkg/eerrs"
	"github.com/openimsdk/protocol/conversation"
	"github.com/openimsdk/protocol/msg"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/openimsdk/tools/apiresp"
	"github.com/openimsdk/tools/errs"
)

const historyScanLimit int64 = 500

type messageHistoryRequest struct {
	ConversationID string `json:"conversationID"`
	Kind           string `json:"kind"`
	Query          string `json:"query"`
	Limit          int    `json:"limit"`
	Cursor         string `json:"cursor"`
}
type messageHistoryResponse struct {
	Messages []map[string]any `json:"messages"`
	HasMore  bool             `json:"hasMore"`
	Cursor   string           `json:"cursor"`
}

// Cursor is a progress marker, not authorization. Every page rechecks membership,
// ownership and current upstream visibility, including deleted messages.
type historyCursor struct {
	Actor        string `json:"a"`
	Conversation string `json:"c"`
	Kind         string `json:"k"`
	Query        string `json:"q"`
	Snapshot     int64  `json:"s"`
	Next         int64  `json:"n"`
}

func validateHistoryRequest(actor string, r *messageHistoryRequest) (*historyCursor, error) {
	if r.ConversationID == "" || len(r.ConversationID) > 256 || len(r.Query) > 256 || len(r.Cursor) > 2048 || r.Limit < 0 || r.Limit > 100 {
		return nil, errs.ErrArgs
	}
	if r.Limit == 0 {
		r.Limit = 50
	}
	if r.Kind == "" {
		r.Kind = "all"
	}
	switch r.Kind {
	case "all", "text", "images", "files", "links":
	default:
		return nil, errs.ErrArgs
	}
	if r.Cursor == "" {
		return nil, nil
	}
	raw, e := base64.RawURLEncoding.DecodeString(r.Cursor)
	if e != nil {
		return nil, errs.ErrArgs
	}
	var c historyCursor
	if json.Unmarshal(raw, &c) != nil || c.Actor != actor || c.Conversation != r.ConversationID || c.Kind != r.Kind || c.Query != r.Query || c.Next < 1 || c.Snapshot < c.Next || c.Snapshot > 9007199254740991 {
		return nil, errs.ErrArgs
	}
	return &c, nil
}

var historyAdmission = struct {
	sync.Mutex
	active map[string]bool
}{active: map[string]bool{}}

func enterHistory(actor string) bool {
	historyAdmission.Lock()
	defer historyAdmission.Unlock()
	if len(historyAdmission.active) >= 8 || historyAdmission.active[actor] {
		return false
	}
	historyAdmission.active[actor] = true
	return true
}
func leaveHistory(actor string) {
	historyAdmission.Lock()
	delete(historyAdmission.active, actor)
	historyAdmission.Unlock()
}
func (o *Api) MessageHistory(c *gin.Context) {
	actor, err := mctx.CheckUser(c)
	if err != nil {
		apiresp.GinError(c, err)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	var req messageHistoryRequest
	if c.ShouldBindJSON(&req) != nil {
		apiresp.GinError(c, errs.ErrArgs)
		return
	}
	if _, err = validateHistoryRequest(actor, &req); err != nil {
		apiresp.GinError(c, err)
		return
	}
	if !enterHistory(actor) {
		apiresp.GinError(c, eerrs.ErrRetryLater.WrapMsg("history search busy"))
		return
	}
	defer leaveHistory(actor)
	result, err := o.messageHistory(c, actor, req)
	if err != nil {
		if errors.Is(err, rtcauthority.ErrPermission) {
			apiresp.GinError(c, errs.ErrNoPermission)
		} else if errors.Is(err, errs.ErrArgs) {
			apiresp.GinError(c, errs.ErrArgs)
		} else {
			apiresp.GinError(c, eerrs.ErrRetryLater.WrapMsg("history search unavailable"))
		}
		return
	}
	apiresp.GinSuccess(c, result)
}
func (o *Api) messageHistory(ctx context.Context, actor string, req messageHistoryRequest) (*messageHistoryResponse, error) {
	cursor, err := validateHistoryRequest(actor, &req)
	if err != nil {
		return nil, err
	}
	if actor == "" {
		return nil, rtcauthority.ErrPermission
	}
	if o.extrasIMURL == "" {
		return nil, errors.New("history service disabled")
	}
	ctx, cancel := context.WithTimeout(rtcOperationContext(ctx), 20*time.Second)
	defer cancel()
	token, err := o.imApiCaller.ImAdminTokenWithDefaultAdmin(ctx)
	if err != nil {
		return nil, err
	}
	ctx = mctx.WithApiToken(ctx, token)
	// Outsiders often have no conversation record. Establish group permission
	// first so a missing record cannot turn a permission denial into a 500.
	checkedGroup := ""
	if strings.HasPrefix(req.ConversationID, "sg_") {
		checkedGroup = strings.TrimPrefix(req.ConversationID, "sg_")
		if checkedGroup == "" {
			return nil, rtcauthority.ErrPermission
		}
		if err = checkMessageExtrasGroupMember(ctx, o.extrasIMURL, o.imApiCaller, checkedGroup, actor); err != nil {
			return nil, err
		}
	}
	conv, err := imapi.NewApiCaller[conversation.GetConversationReq, conversation.GetConversationResp]("/conversation/get_conversation").Call(ctx, o.extrasIMURL, &conversation.GetConversationReq{OwnerUserID: actor, ConversationID: req.ConversationID})
	if err != nil {
		return nil, err
	}
	if conv.Conversation == nil || conv.Conversation.OwnerUserID != actor || conv.Conversation.ConversationID != req.ConversationID {
		return nil, rtcauthority.ErrPermission
	}
	if checkedGroup != "" && conv.Conversation.GroupID != checkedGroup {
		return nil, rtcauthority.ErrPermission
	}
	if gid := conv.Conversation.GroupID; gid != "" && gid != checkedGroup {
		if err = checkMessageExtrasGroupMember(ctx, o.extrasIMURL, o.imApiCaller, gid, actor); err != nil {
			return nil, err
		}
	}
	progress, err := imapi.NewApiCaller[msg.GetConversationsHasReadAndMaxSeqReq, msg.GetConversationsHasReadAndMaxSeqResp]("/msg/get_conversations_has_read_and_max_seq").Call(ctx, o.extrasIMURL, &msg.GetConversationsHasReadAndMaxSeqReq{UserID: actor, ConversationIDs: []string{req.ConversationID}})
	if err != nil {
		return nil, err
	}
	seq := progress.Seqs[req.ConversationID]
	if seq == nil || seq.MaxSeq < 0 {
		return nil, errors.New("missing history bounds")
	}
	maximum := seq.MaxSeq
	if max := conv.Conversation.MaxSeq; max > 0 && max < maximum {
		maximum = max
	}
	if cursor == nil {
		cursor = &historyCursor{Actor: actor, Conversation: req.ConversationID, Kind: req.Kind, Query: req.Query, Snapshot: maximum, Next: maximum}
	}
	if cursor.Snapshot > maximum {
		cursor.Next = min(cursor.Next, maximum)
	}
	result := &messageHistoryResponse{Messages: []map[string]any{}}
	lower := max(int64(1), conv.Conversation.MinSeq)
	end := cursor.Next
	if end < lower {
		return result, nil
	}
	begin := max(lower, end-historyScanLimit+1)
	// v3.8.3-patch.18 GetMsgBySeqsRange enforces actor min/max and
	// GetMessageBySeqs projects per-user deletions and revocations. Never use
	// admin SearchMessage, whose search bypasses this user projection.
	source, err := imapi.NewApiCaller[sdkws.PullMessageBySeqsReq, sdkws.PullMessageBySeqsResp]("/msg/pull_msg_by_seq").Call(ctx, o.extrasIMURL, &sdkws.PullMessageBySeqsReq{UserID: actor, Order: sdkws.PullOrder_PullOrderDesc, SeqRanges: []*sdkws.SeqRange{{ConversationID: req.ConversationID, Begin: begin, End: end, Num: end - begin + 1}}})
	if err != nil {
		return nil, err
	}
	page := source.Msgs[req.ConversationID]
	// Upstream swallows storage errors and omits the map entry; do not advance
	// across such a page. In-bounds deleted/missing records are placeholders.
	if page == nil || len(page.Msgs) == 0 {
		return nil, errors.New("incomplete history page")
	}
	sort.Slice(page.Msgs, func(i, j int) bool { return page.Msgs[i].GetSeq() > page.Msgs[j].GetSeq() })
	// The deployed pull contract emits one record or deleted placeholder per
	// visible sequence. Reject truncated pages before returning any matches.
	expected := end
	for _, m := range page.Msgs {
		if m == nil || m.Seq != expected || m.Seq < begin {
			return nil, errors.New("incomplete history sequences")
		}
		expected--
	}
	if expected >= begin && !page.IsEnd {
		return nil, errors.New("truncated history page")
	}
	records := map[string]*messageextras.Record{}
	if o.extrasRedis != nil {
		ids := []string{}
		for _, m := range page.Msgs {
			if m != nil && m.ClientMsgID != "" {
				ids = append(ids, m.ClientMsgID)
			}
		}
		if len(ids) > 0 {
			digest := sha256.Sum256([]byte(req.ConversationID))
			key := "im:message-extras:v1:" + hex.EncodeToString(digest[:])
			values, e := o.extrasRedis.HMGet(ctx, key, ids...).Result()
			if e != nil {
				return nil, e
			}
			for i, val := range values {
				if val != nil {
					s, ok := val.(string)
					if !ok {
						return nil, errors.New("invalid history metadata")
					}
					var r messageextras.Record
					if e = json.Unmarshal([]byte(s), &r); e != nil {
						return nil, e
					}
					records[ids[i]] = &r
				}
			}
		}
	}
	next := begin - 1
	exhausted := true
	seen := map[int64]bool{}
	for _, m := range page.Msgs {
		if m == nil || m.Seq < begin || m.Seq > end || seen[m.Seq] {
			return nil, errors.New("invalid history sequence")
		}
		seen[m.Seq] = true
		normalized, ok := projectHistoryMessage(m, records[m.ClientMsgID], req)
		if ok {
			result.Messages = append(result.Messages, normalized)
		}
		if len(result.Messages) >= req.Limit {
			next = m.Seq - 1
			exhausted = next < begin
			break
		}
	}
	result.HasMore = next >= lower && !(exhausted && page.IsEnd)
	if result.HasMore {
		cursor.Next = next
		raw, _ := json.Marshal(cursor)
		result.Cursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return result, nil
}

var historyURL = regexp.MustCompile(`(?i)https?://[^\s<>]+`)

func projectHistoryMessage(m *sdkws.MsgData, record *messageextras.Record, r messageHistoryRequest) (map[string]any, bool) {
	if m == nil || m.ClientMsgID == "" || m.Status >= 4 || m.ContentType >= 1000 {
		return nil, false
	}
	var elem map[string]any
	if json.Unmarshal(m.Content, &elem) != nil || elem == nil {
		return nil, false
	}
	var field, text string
	switch m.ContentType {
	case 101:
		field = "textElem"
		text, _ = elem["content"].(string)
		if record != nil && record.Text != nil {
			text = *record.Text
			elem["content"] = text
		}
	case 102:
		field = "pictureElem"
	case 103:
		field = "soundElem"
	case 104:
		field = "videoElem"
	case 105:
		field = "fileElem"
		text, _ = elem["fileName"].(string)
	case 106:
		field = "atTextElem"
		text, _ = elem["text"].(string)
	case 108:
		field = "cardElem"
		nickname, _ := elem["nickname"].(string)
		userID, _ := elem["userID"].(string)
		text = nickname + " " + userID
	case 109:
		field = "locationElem"
		text, _ = elem["description"].(string)
	case 114:
		field = "quoteElem"
		text, _ = elem["text"].(string)
	default:
		field = "customElem"
	}
	if (m.ContentType == 106 || m.ContentType == 114) && record != nil && record.Text != nil {
		text = *record.Text
		elem["text"] = text
	}
	switch r.Kind {
	case "text":
		if m.ContentType != 101 && m.ContentType != 106 && m.ContentType != 114 {
			return nil, false
		}
	case "images":
		if m.ContentType != 102 {
			return nil, false
		}
	case "files":
		if m.ContentType != 105 {
			return nil, false
		}
	case "links":
		if !historyURL.MatchString(text) {
			return nil, false
		}
	}
	if r.Query != "" && !strings.Contains(strings.ToLower(text), strings.ToLower(r.Query)) {
		return nil, false
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil {
		return nil, false
	}
	content, _ := json.Marshal(elem)
	result["status"] = 2
	result["content"] = string(content)
	result[field] = elem
	if record != nil {
		result["projection"] = record
	}
	return result, true
}
