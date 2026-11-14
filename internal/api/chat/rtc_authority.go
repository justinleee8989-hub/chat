package chat

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/openimsdk/chat/pkg/common/imapi"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/chat/pkg/common/rtcauthority"
	"github.com/openimsdk/protocol/constant"
	"github.com/openimsdk/protocol/group"
	"github.com/openimsdk/protocol/user"
	"github.com/openimsdk/tools/apiresp"
	"github.com/openimsdk/tools/errs"
	"github.com/redis/go-redis/v9"
	"net/http"
	"os"
	"strings"
)

const rtcMaxBodyBytes int64 = 16 << 10

func (o *Api) initRTC(ctx context.Context, imURL string) error {
	rtcEnabled := os.Getenv("RTC_ENABLED") == "true"
	extrasAddr, extrasPassword := os.Getenv("MESSAGE_EXTRAS_REDIS_ADDR"), os.Getenv("MESSAGE_EXTRAS_REDIS_PASSWORD")
	if extrasAddr == "" {
		extrasAddr = os.Getenv("RTC_REDIS_ADDR")
	}
	if extrasPassword == "" {
		extrasPassword = os.Getenv("RTC_REDIS_PASSWORD")
	}
	if messageExtrasEnabled(os.Getenv("MESSAGE_EXTRAS_ENABLED"), rtcEnabled) {
		if extrasAddr == "" {
			return errors.New("message extras enabled without Redis address")
		}
		rdb := redis.NewClient(&redis.Options{Addr: extrasAddr, Password: extrasPassword})
		if err := rdb.Ping(ctx).Err(); err != nil {
			_ = rdb.Close()
			return err
		}
		o.extrasRedis = rdb
		o.extrasIMURL = imURL
	}
	if !rtcEnabled {
		return nil
	}
	addr, key, secret := os.Getenv("RTC_REDIS_ADDR"), os.Getenv("RTC_KEY"), os.Getenv("RTC_SECRET")
	if addr == "" || key == "" || secret == "" || os.Getenv("RTC_PUBLIC_URL") == "" || os.Getenv("RTC_INTERNAL_URL") == "" {
		return errors.New("incomplete RTC configuration")
	}
	var rdb *redis.Client
	if o.extrasRedis != nil && addr == extrasAddr && os.Getenv("RTC_REDIS_PASSWORD") == extrasPassword {
		rdb = o.extrasRedis
	} else {
		rdb = redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("RTC_REDIS_PASSWORD")})
		if err := rdb.Ping(ctx).Err(); err != nil {
			_ = rdb.Close()
			return err
		}
	}
	svc := &rtcauthority.Service{Store: &rtcauthority.Store{Redis: rdb, Key: "rtc:authority:v1"}, Key: key, Secret: secret, PublicURL: os.Getenv("RTC_PUBLIC_URL"), InternalURL: os.Getenv("RTC_INTERNAL_URL")}
	svc.UserExists = func(ctx context.Context, id string) error {
		ctx = rtcOperationContext(ctx)
		token, err := o.imApiCaller.ImAdminTokenWithDefaultAdmin(ctx)
		if err != nil {
			return err
		}
		result, err := imapi.NewApiCaller[user.GetDesignateUsersReq, user.GetDesignateUsersResp]("/user/get_users_info").Call(mctx.WithApiToken(ctx, token), imURL, &user.GetDesignateUsersReq{UserIDs: []string{id}})
		if err != nil {
			return err
		}
		if len(result.UsersInfo) != 1 || result.UsersInfo[0].UserID != id {
			return rtcauthority.ErrPermission
		}
		return nil
	}
	svc.GroupMember = func(ctx context.Context, gid, user string) error {
		return checkMessageExtrasGroupMember(ctx, imURL, o.imApiCaller, gid, user)
	}
	o.rtcAuthority = svc
	go svc.Run(ctx)
	return nil
}

func messageExtrasEnabled(configured string, rtcEnabled bool) bool {
	if configured == "" {
		return rtcEnabled
	}
	return strings.EqualFold(configured, "true")
}

func checkMessageExtrasGroupMember(ctx context.Context, imURL string, caller imapi.CallerInterface, gid, user string) error {
	ctx = rtcOperationContext(ctx)
	token, err := caller.ImAdminTokenWithDefaultAdmin(ctx)
	if err != nil {
		return err
	}
	ctx = mctx.WithApiToken(ctx, token)
	info, err := caller.FindGroupInfo(ctx, []string{gid})
	if err != nil {
		return err
	}
	if len(info) != 1 || info[0].Status == constant.GroupStatusDismissed {
		return rtcauthority.ErrPermission
	}
	result, err := imapi.NewApiCaller[group.GetGroupMembersInfoReq, group.GetGroupMembersInfoResp]("/group/get_group_members_info").Call(ctx, imURL, &group.GetGroupMembersInfoReq{GroupID: gid, UserIDs: []string{user}})
	if err != nil {
		return err
	}
	if len(result.Members) != 1 || result.Members[0] == nil || result.Members[0].UserID != user {
		return rtcauthority.ErrPermission
	}
	return nil
}

func validateRTCRequest(req rtcauthority.Request) error {
	if len(req.Action) > 32 || len(req.Room) > 256 || len(req.Peer) > 256 || len(req.GroupID) > 256 || len(req.RequestID) > 128 {
		return errs.ErrArgs
	}
	return nil
}

func (o *Api) RTCCall(c *gin.Context) {
	user, err := mctx.CheckUser(c)
	if err != nil {
		apiresp.GinError(c, err)
		return
	}
	if o.rtcAuthority == nil {
		apiresp.GinError(c, errs.ErrNoPermission.WrapMsg("通话服务未启用"))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, rtcMaxBodyBytes)
	var req rtcauthority.Request
	if err = c.ShouldBindJSON(&req); err != nil || validateRTCRequest(req) != nil {
		apiresp.GinError(c, errs.ErrArgs.WrapMsg("请求格式错误"))
		return
	}
	result, err := o.rtcAuthority.Execute(c, user, req)
	if err != nil {
		apiresp.GinError(c, errs.ErrNoPermission.WrapMsg(err.Error()))
		return
	}
	apiresp.GinSuccess(c, map[string]any{"calls": result})
}
func (o *Api) RTCToken(c *gin.Context) {
	user, err := mctx.CheckUser(c)
	if err != nil {
		apiresp.GinError(c, err)
		return
	}
	if o.rtcAuthority == nil {
		apiresp.GinError(c, errs.ErrNoPermission.WrapMsg("通话服务未启用"))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, rtcMaxBodyBytes)
	var req struct {
		Room string `json:"room"`
	}
	if err = c.ShouldBindJSON(&req); err != nil || validateRTCRequest(rtcauthority.Request{Action: "token", Room: req.Room}) != nil {
		apiresp.GinError(c, errs.ErrArgs.WrapMsg("请求格式错误"))
		return
	}
	token, err := o.rtcAuthority.Token(c, req.Room, user)
	if err != nil {
		apiresp.GinError(c, errs.ErrNoPermission.WrapMsg(err.Error()))
		return
	}
	apiresp.GinSuccess(c, map[string]string{"token": token, "serverUrl": o.rtcAuthority.PublicURL})
}
func (o *Api) RTCWebhook(c *gin.Context) {
	if o.rtcAuthority == nil {
		c.Status(503)
		return
	}
	o.rtcAuthority.Webhook(c.Writer, c.Request)
}

func rtcOperationContext(ctx context.Context) context.Context {
	if value, _ := ctx.Value(constant.OperationID).(string); value != "" {
		return ctx
	}
	return context.WithValue(ctx, constant.OperationID, uuid.NewString())
}

// LiveKit signs webhook bodies; it does not send OpenIM's tracing header.
func rtcWebhookOperationID(c *gin.Context) {
	if c.Request.URL.Path == "/rtc/webhook" && c.GetHeader(constant.OperationID) == "" {
		c.Request.Header.Set(constant.OperationID, uuid.NewString())
	}
	c.Next()
}
