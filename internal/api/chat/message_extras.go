package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/openimsdk/chat/pkg/common/imapi"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/chat/pkg/common/messageextras"
	"github.com/openimsdk/protocol/conversation"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/openimsdk/tools/apiresp"
	"github.com/openimsdk/tools/errs"
	"github.com/redis/go-redis/v9"
	"net/http"
	"time"
)

const messageExtrasMaxBodyBytes int64 = 128 << 10

// Metadata is separate from immutable OpenIM messages; every access is checked against OpenIM.
func (o *Api) MessageExtras(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, messageExtrasMaxBodyBytes)
	actor, err := mctx.CheckUser(c)
	if err != nil {
		apiresp.GinError(c, err)
		return
	}
	var req messageextras.Request
	if c.ShouldBindJSON(&req) != nil || len(req.ConversationID) > 256 || req.ConversationID == "" || len(req.MessageIDs) > 100 || len(req.MessageID) > 128 {
		apiresp.GinError(c, errs.ErrArgs)
		return
	}
	for _, messageID := range req.MessageIDs {
		if messageID == "" || len(messageID) > 128 {
			apiresp.GinError(c, errs.ErrArgs)
			return
		}
	}
	requestCtx := c.Request.Context()
	if req.Action == "get" {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(requestCtx, 9*time.Second)
		defer cancel()
		if err := acquireProgressSlot(requestCtx, extrasSlots); err != nil {
			apiresp.GinError(c, errs.ErrInternalServer.WrapMsg("消息增强查询繁忙，请稍后重试"))
			return
		}
		defer func() { <-extrasSlots }()
	}
	result, err := o.messageExtras(requestCtx, actor, req)
	if err != nil {
		apiresp.GinError(c, errs.ErrNoPermission.WrapMsg(err.Error()))
		return
	}
	apiresp.GinSuccess(c, map[string]any{"records": result})
}
func (o *Api) messageExtras(ctx context.Context, actor string, req messageextras.Request) (map[string]*messageextras.Record, error) {
	if o.extrasRedis == nil {
		return nil, errors.New("消息增强服务未启用")
	}
	ctx, cancel := context.WithTimeout(rtcOperationContext(ctx), 9*time.Second)
	defer cancel()
	token, err := o.imApiCaller.ImAdminTokenWithDefaultAdmin(ctx)
	if err != nil {
		return nil, err
	}
	ctx = mctx.WithApiToken(ctx, token)
	conv, err := imapi.NewApiCaller[conversation.GetConversationReq, conversation.GetConversationResp]("/conversation/get_conversation").Call(ctx, o.extrasIMURL, &conversation.GetConversationReq{OwnerUserID: actor, ConversationID: req.ConversationID})
	if err != nil {
		return nil, err
	}
	if conv.Conversation == nil || conv.Conversation.OwnerUserID != actor || conv.Conversation.ConversationID != req.ConversationID {
		return nil, messageextras.ErrPermission
	}
	if gid := conv.Conversation.GroupID; gid != "" {
		if err = checkMessageExtrasGroupMember(ctx, o.extrasIMURL, o.imApiCaller, gid, actor); err != nil {
			return nil, err
		}
	}
	digest := sha256.Sum256([]byte(req.ConversationID))
	key := "im:message-extras:v1:" + hex.EncodeToString(digest[:])
	result := map[string]*messageextras.Record{}
	if req.Action == "get" {
		if len(req.MessageIDs) == 0 {
			return result, nil
		}
		values, err := o.extrasRedis.HMGet(ctx, key, req.MessageIDs...).Result()
		if err != nil {
			return nil, err
		}
		for i, val := range values {
			if val != nil {
				var record messageextras.Record
				raw, ok := val.(string)
				if !ok {
					return nil, errors.New("invalid message metadata")
				}
				if err = json.Unmarshal([]byte(raw), &record); err != nil {
					return nil, err
				}
				result[req.MessageIDs[i]] = &record
			}
		}
		return o.visibleExtraRecords(ctx, actor, req.ConversationID, conv.Conversation.MinSeq, conv.Conversation.MaxSeq, result)
	}
	if req.MessageID == "" || req.Seq < 1 {
		return nil, errors.New("消息尚未同步，请稍后重试")
	}
	source, err := imapi.NewApiCaller[sdkws.PullMessageBySeqsReq, sdkws.PullMessageBySeqsResp]("/msg/pull_msg_by_seq").Call(ctx, o.extrasIMURL, &sdkws.PullMessageBySeqsReq{UserID: actor, SeqRanges: []*sdkws.SeqRange{{ConversationID: req.ConversationID, Begin: req.Seq, End: req.Seq, Num: 1}}})
	if err != nil {
		return nil, err
	}
	var original *sdkws.MsgData
	if page := source.Msgs[req.ConversationID]; page != nil {
		for _, msg := range page.Msgs {
			if msg.ClientMsgID == req.MessageID && msg.Seq == req.Seq {
				original = msg
			}
		}
	}
	if original == nil || original.Status >= 4 || original.ContentType >= 1000 {
		return nil, messageextras.ErrPermission
	}
	for attempt := 0; attempt < 16; attempt++ {
		err = o.extrasRedis.Watch(ctx, func(tx *redis.Tx) error {
			record := &messageextras.Record{}
			raw, e := tx.HGet(ctx, key, req.MessageID).Result()
			if e != nil && e != redis.Nil {
				return e
			}
			if e == nil {
				if e = json.Unmarshal([]byte(raw), record); e != nil {
					return e
				}
			}
			if e = record.ApplyWithPolicy(actor, original.SendID, original.ContentType, req, time.Now().UnixMilli(), o.reactionPolicy); e != nil {
				return e
			}
			record.Seq = original.Seq
			data, e := json.Marshal(record)
			if e != nil {
				return e
			}
			_, e = tx.TxPipelined(ctx, func(p redis.Pipeliner) error { p.HSet(ctx, key, req.MessageID, data); return nil })
			if e == nil {
				result[req.MessageID] = record
			}
			return e
		}, key)
		if err != redis.TxFailedErr {
			return result, err
		}
	}
	return nil, messageextras.ErrConflict
}
