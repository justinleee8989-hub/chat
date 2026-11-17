package chat

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/openimsdk/chat/pkg/common/imapi"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/openimsdk/tools/log"
	"github.com/redis/go-redis/v9"
)

// OpenIM moves a group member's read position without telling anyone else, so a
// sender's "N 人已读" under their message never changed while they watched. With
// OpenIM's afterGroupMsgRead webhook pointed here, each read is turned into an
// online-only business notification to the senders of the messages it covers;
// their clients then ask /user/message/readers again.
//
// OpenIM is configured with
//
//	webhooks.url: http://<chat-api>/callback/openim_webhook/<OPENIM_WEBHOOK_SECRET>
//	webhooks.afterGroupMsgRead.enable: true
//
// and posts to <url>/<callbackCommand>. Without OPENIM_WEBHOOK_SECRET the route
// answers 404. The read webhook needs OpenIM to send conversationID, groupID and
// hasReadSeq (open-im-server feat/group-read-webhook-context), and message extras
// enabled here (MESSAGE_EXTRAS_ENABLED, or RTC): that is what sets the OpenIM API
// URL and the Redis this uses, as /user/message/readers does.
const (
	groupReadWebhookCommand    = "callbackAfterGroupMsgReadCommand"
	groupReadNotificationKey   = "group_read_progress"
	groupReadLookback          = 20
	groupReadThrottleInterval  = time.Second
	groupReadThrottleKeyPrefix = "chat:group_read_notify:"
)

type groupReadWebhookReq struct {
	SendID         string `json:"sendID"`
	ConversationID string `json:"conversationID"`
	GroupID        string `json:"groupID"`
	HasReadSeq     int64  `json:"hasReadSeq"`
}

// groupReadNotice is the business notification's data, read by the app.
type groupReadNotice struct {
	ConversationID string `json:"conversationID"`
	ReaderID       string `json:"readerID"`
	HasReadSeq     int64  `json:"hasReadSeq"`
}

type businessNotificationReq struct {
	Key              string `json:"key"`
	Data             string `json:"data"`
	SendUserID       string `json:"sendUserID"`
	RecvUserID       string `json:"recvUserID"`
	SendMsg          bool   `json:"sendMsg"`
	ReliabilityLevel int    `json:"reliabilityLevel"`
}

type businessNotificationResp struct{}

// groupReadLimiter lets one notification per conversation and sender through
// each interval: a member scrolling through a busy group moves their read
// position many times a second. Kept in Redis so every chat-api instance shares it.
type groupReadLimiter interface {
	allow(ctx context.Context, key string) (bool, error)
}

type redisGroupReadLimiter struct{ rdb *redis.Client }

func (l redisGroupReadLimiter) allow(ctx context.Context, key string) (bool, error) {
	return l.rdb.SetNX(ctx, groupReadThrottleKeyPrefix+key, 1, groupReadThrottleInterval).Result()
}

func (o *Api) groupReadLimit() groupReadLimiter {
	if o.groupReadLimiter != nil {
		return o.groupReadLimiter
	}
	return redisGroupReadLimiter{rdb: o.extrasRedis}
}

func openIMWebhookAuthorized(given string) bool {
	secret := os.Getenv("OPENIM_WEBHOOK_SECRET")
	return secret != "" && subtle.ConstantTimeCompare([]byte(given), []byte(secret)) == 1
}

func (o *Api) OpenIMWebhook(c *gin.Context) {
	if !openIMWebhookAuthorized(c.Param("secret")) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	// Every enabled OpenIM after-webhook lands here; only the group read one is used.
	var req groupReadWebhookReq
	if c.Param("command") == groupReadWebhookCommand && c.ShouldBindJSON(&req) == nil && validGroupRead(req) {
		if o.extrasIMURL == "" || o.extrasRedis == nil {
			o.groupReadDisabledOnce.Do(func() {
				log.ZWarn(c, "group read webhook received but message extras are disabled; no read progress is pushed", nil)
			})
		} else {
			// Answered at once: OpenIM queues its after-webhooks and waits for each.
			go func() {
				ctx, cancel := context.WithTimeout(rtcOperationContext(context.Background()), 10*time.Second)
				defer cancel()
				if err := o.notifyGroupReadProgress(ctx, req); err != nil {
					log.ZWarn(ctx, "group read notification failed", err, "conversationID", req.ConversationID)
				}
			}()
		}
	}
	c.JSON(http.StatusOK, gin.H{"actionCode": 0, "errCode": 0, "errMsg": "", "errDlt": "", "nextCode": 0})
}

func validGroupRead(req groupReadWebhookReq) bool {
	return req.SendID != "" && req.GroupID != "" && req.HasReadSeq > 0 &&
		req.ConversationID == "sg_"+req.GroupID && len(req.ConversationID) <= 256
}

// notifyGroupReadProgress tells the senders of the last messages up to the new
// read position that it moved.
func (o *Api) notifyGroupReadProgress(ctx context.Context, req groupReadWebhookReq) error {
	token, err := o.imApiCaller.ImAdminTokenWithDefaultAdmin(ctx)
	if err != nil {
		return err
	}
	ctx = mctx.WithApiToken(ctx, token)
	begin := max(int64(1), req.HasReadSeq-groupReadLookback+1)
	source, err := imapi.NewApiCaller[sdkws.PullMessageBySeqsReq, sdkws.PullMessageBySeqsResp]("/msg/pull_msg_by_seq").Call(ctx, o.extrasIMURL, &sdkws.PullMessageBySeqsReq{
		UserID:    req.SendID,
		SeqRanges: []*sdkws.SeqRange{{ConversationID: req.ConversationID, Begin: begin, End: req.HasReadSeq, Num: req.HasReadSeq - begin + 1}},
	})
	if err != nil {
		return err
	}
	senders := map[string]bool{}
	if pulled := source.Msgs[req.ConversationID]; pulled != nil {
		for _, m := range pulled.Msgs {
			if m != nil && m.GroupID == req.GroupID && m.SendID != "" && m.SendID != req.SendID && m.ContentType < 1000 && m.Status < 4 {
				senders[m.SendID] = true
			}
		}
	}
	data, err := json.Marshal(groupReadNotice{ConversationID: req.ConversationID, ReaderID: req.SendID, HasReadSeq: req.HasReadSeq})
	if err != nil {
		return err
	}
	var firstErr error
	limit := o.groupReadLimit()
	for sender := range senders {
		allowed, err := limit.allow(ctx, req.ConversationID+":"+sender)
		if err != nil {
			// A Redis hiccup costs at most a duplicate notice, never a missed one.
			log.ZWarn(ctx, "group read throttle unavailable", err, "conversationID", req.ConversationID)
		} else if !allowed {
			continue
		}
		_, err = imapi.NewApiCaller[businessNotificationReq, businessNotificationResp]("/msg/send_business_notification").Call(ctx, o.extrasIMURL, &businessNotificationReq{
			Key:        groupReadNotificationKey,
			Data:       string(data),
			SendUserID: o.ImUserID,
			RecvUserID: sender,
			// Online only: a client that is not watching reads the count when it opens the chat.
			ReliabilityLevel: 1,
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
