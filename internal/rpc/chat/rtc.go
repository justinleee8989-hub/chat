package chat

import (
	"context"
	"github.com/openimsdk/chat/pkg/protocol/chat"
	"github.com/openimsdk/tools/errs"
)

// All room credentials must pass through the authenticated RTC authority API.
func (o *chatSvr) GetTokenForVideoMeeting(ctx context.Context, req *chat.GetTokenForVideoMeetingReq) (*chat.GetTokenForVideoMeetingResp, error) {
	return nil, errs.ErrNoPermission.WrapMsg("请使用经过房间授权的通话接口")
}
