package chat

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/openimsdk/chat/pkg/common/imapi"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/chat/pkg/common/rtcauthority"
	"github.com/openimsdk/chat/pkg/eerrs"
	"github.com/openimsdk/protocol/conversation"
	"github.com/openimsdk/protocol/group"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/openimsdk/tools/apiresp"
	"github.com/openimsdk/tools/errs"
	"net/http"
	"strings"
	"time"
)

type messageReadersRequest struct {
	ConversationID string `json:"conversationID"`
	MessageID      string `json:"messageID"`
	Seq            int64  `json:"seq"`
}
type messageReader struct {
	UserID   string `json:"userID"`
	Nickname string `json:"nickname"`
	FaceURL  string `json:"faceURL"`
	Read     bool   `json:"read"`
}
type messageReadersResponse struct {
	Members             []messageReader `json:"members"`
	ReadCount           int             `json:"readCount"`
	UnreadCount         int             `json:"unreadCount"`
	ExcludedJoinedLater int             `json:"excludedJoinedLater"`
}

func (o *Api) MessageReaders(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	actor, err := mctx.CheckUser(c)
	if err != nil {
		apiresp.GinError(c, err)
		return
	}
	var req messageReadersRequest
	if c.ShouldBindJSON(&req) != nil || req.ConversationID == "" || len(req.ConversationID) > 256 || req.MessageID == "" || len(req.MessageID) > 128 || req.Seq < 1 {
		apiresp.GinError(c, errs.ErrArgs)
		return
	}
	if !readersAdmission.enter(actor) {
		apiresp.GinError(c, eerrs.ErrRetryLater.WrapMsg("read details busy"))
		return
	}
	defer readersAdmission.leave(actor)
	result, err := o.groupMessageReaders(c.Request.Context(), actor, req)
	if err != nil {
		apiresp.GinError(c, readersError(err))
		return
	}
	apiresp.GinSuccess(c, result)
}

func (o *Api) groupMessageReaders(ctx context.Context, actor string, req messageReadersRequest) (*messageReadersResponse, error) {
	if o.extrasIMURL == "" {
		return nil, errReadersDisabled
	}
	ctx, cancel := context.WithTimeout(rtcOperationContext(ctx), 25*time.Second)
	defer cancel()
	token, err := o.imApiCaller.ImAdminTokenWithDefaultAdmin(ctx)
	if err != nil {
		return nil, err
	}
	ctx = mctx.WithApiToken(ctx, token)
	// A nonmember may have no conversation record at all. Check membership
	// before querying that record, while preserving genuine upstream failures.
	gid := strings.TrimPrefix(req.ConversationID, "sg_")
	if gid == req.ConversationID || gid == "" {
		return nil, rtcauthority.ErrPermission
	}
	if err = checkMessageExtrasGroupMember(ctx, o.extrasIMURL, o.imApiCaller, gid, actor); err != nil {
		return nil, err
	}
	conv, err := imapi.NewApiCaller[conversation.GetConversationReq, conversation.GetConversationResp]("/conversation/get_conversation").Call(ctx, o.extrasIMURL, &conversation.GetConversationReq{OwnerUserID: actor, ConversationID: req.ConversationID})
	if err != nil {
		return nil, err
	}
	if conv.Conversation == nil || conv.Conversation.OwnerUserID != actor || conv.Conversation.GroupID != gid {
		return nil, rtcauthority.ErrPermission
	}
	source, err := imapi.NewApiCaller[sdkws.PullMessageBySeqsReq, sdkws.PullMessageBySeqsResp]("/msg/pull_msg_by_seq").Call(ctx, o.extrasIMURL, &sdkws.PullMessageBySeqsReq{UserID: actor, SeqRanges: []*sdkws.SeqRange{{ConversationID: req.ConversationID, Begin: req.Seq, End: req.Seq, Num: 1}}})
	if err != nil {
		return nil, err
	}
	var original *sdkws.MsgData
	if p := source.Msgs[req.ConversationID]; p != nil {
		for _, m := range p.Msgs {
			if m.ClientMsgID == req.MessageID && m.Seq == req.Seq {
				original = m
			}
		}
	}
	if original == nil || original.SendID != actor || original.GroupID != gid || original.Status >= 4 || original.ContentType >= 1000 {
		return nil, rtcauthority.ErrPermission
	}
	result := &messageReadersResponse{Members: []messageReader{}}
	seen := map[string]bool{}
	for page := int32(1); ; page++ {
		members, e := imapi.NewApiCaller[group.GetGroupMemberListReq, group.GetGroupMemberListResp]("/group/get_group_member_list").Call(ctx, o.extrasIMURL, &group.GetGroupMemberListReq{GroupID: gid, Pagination: &sdkws.RequestPagination{PageNumber: page, ShowNumber: 100}})
		if e != nil {
			return nil, e
		}
		if members.Total > 2000 || page > 20 {
			return nil, errReaderLimit
		}
		for _, m := range members.Members {
			if m.UserID == actor || seen[m.UserID] {
				continue
			}
			seen[m.UserID] = true
			if m.JoinTime > original.SendTime {
				result.ExcludedJoinedLater++
				continue
			}
			result.Members = append(result.Members, messageReader{UserID: m.UserID, Nickname: m.Nickname, FaceURL: m.FaceURL})
		}
		if int(page)*100 >= int(members.Total) {
			break
		}
		if len(members.Members) == 0 {
			return nil, errMemberListIncomplete
		}
	}
	if err = o.loadReaderProgress(ctx, req.ConversationID, req.Seq, result.Members); err != nil {
		return nil, err
	}

	for _, m := range result.Members {
		if m.Read {
			result.ReadCount++
		} else {
			result.UnreadCount++
		}
	}
	return result, nil
}
