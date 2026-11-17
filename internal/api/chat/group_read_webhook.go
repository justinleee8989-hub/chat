package chat

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/openimsdk/chat/pkg/common/imapi"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/openimsdk/tools/log"
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
// hasReadSeq (open-im-server feat/group-read-webhook-context).
const (
	groupReadWebhookCommand   = "callbackAfterGroupMsgReadCommand"
	groupReadNotificationKey  = "group_read_progress"
	groupReadLookback         = 20
	groupReadThrottleInterval = time.Second
	groupReadThrottleMaxKeys  = 10000
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

// groupReadThrottle lets one notification per conversation and sender through
// each interval: a member scrolling through a busy group moves their read
// position many times a second.
type groupReadThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (t *groupReadThrottle) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	if at, ok := t.last[key]; ok && now.Sub(at) < groupReadThrottleInterval {
		return false
	}
	if len(t.last) >= groupReadThrottleMaxKeys {
		for k, at := range t.last {
			if now.Sub(at) >= groupReadThrottleInterval {
				delete(t.last, k)
			}
		}
	}
	t.last[key] = now
	return true
}

var groupReadNotifications groupReadThrottle

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
		// Answered at once: OpenIM queues its after-webhooks and waits for each.
		go func() {
			ctx, cancel := context.WithTimeout(rtcOperationContext(context.Background()), 10*time.Second)
			defer cancel()
			if err := o.notifyGroupReadProgress(ctx, req, time.Now()); err != nil {
				log.ZWarn(ctx, "group read notification failed", err, "conversationID", req.ConversationID)
			}
		}()
	}
	c.JSON(http.StatusOK, gin.H{"actionCode": 0, "errCode": 0, "errMsg": "", "errDlt": "", "nextCode": 0})
}

func validGroupRead(req groupReadWebhookReq) bool {
	return req.SendID != "" && req.GroupID != "" && req.HasReadSeq > 0 &&
		req.ConversationID == "sg_"+req.GroupID && len(req.ConversationID) <= 256
}

// notifyGroupReadProgress tells the senders of the last messages up to the new
// read position that it moved.
func (o *Api) notifyGroupReadProgress(ctx context.Context, req groupReadWebhookReq, now time.Time) error {
	if o.extrasIMURL == "" {
		return nil
	}
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
	for sender := range senders {
		if !groupReadNotifications.allow(req.ConversationID+"\x00"+sender, now) {
			continue
		}
		_, err := imapi.NewApiCaller[businessNotificationReq, businessNotificationResp]("/msg/send_business_notification").Call(ctx, o.extrasIMURL, &businessNotificationReq{
			Key:        groupReadNotificationKey,
			Data:       string(data),
			SendUserID: o.notificationSenderID(),
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

func (o *Api) notificationSenderID() string {
	if o.Api != nil && strings.TrimSpace(o.ImUserID) != "" {
		return o.ImUserID
	}
	return "imAdmin"
}
