package chat

import (
	"context"
	"errors"
	"github.com/openimsdk/chat/pkg/common/callprotocol"

	"github.com/openimsdk/chat/pkg/common/apistruct"
	"github.com/openimsdk/chat/pkg/common/imapi"
	"github.com/openimsdk/chat/pkg/common/mctx"
	"github.com/openimsdk/chat/pkg/common/rtcauthority"
	"github.com/openimsdk/protocol/constant"
	"github.com/openimsdk/protocol/msg"
)

func rtcRecordRequest(record rtcauthority.TerminalRecord) (*apistruct.SendMsgReq, error) {
	content, err := callprotocol.Encode(record)
	if err != nil {
		return nil, err
	}
	return &apistruct.SendMsgReq{RecvID: record.Callee, SendMsg: apistruct.SendMsg{
		SendID: record.Caller, ContentType: constant.Text, SessionType: constant.SingleChatType,
		Content: map[string]any{"content": content}, SendTime: record.Ended,
	}}, nil
}

func (o *Api) sendRTCRecord(ctx context.Context, imURL string, record rtcauthority.TerminalRecord) error {
	req, err := rtcRecordRequest(record)
	if err != nil {
		return err
	}
	ctx = rtcOperationContext(ctx)
	token, err := o.imApiCaller.ImAdminTokenWithDefaultAdmin(ctx)
	if err != nil {
		return err
	}
	// /msg/send_msg generates its own clientMsgID. Stable eventID in content
	// enables room/event deduplication after an ambiguous response or worker crash.
	resp, err := imapi.NewApiCaller[apistruct.SendMsgReq, msg.SendMsgResp]("/msg/send_msg").Call(mctx.WithApiToken(ctx, token), imURL, req)
	if err == nil && (resp == nil || resp.ClientMsgID == "") {
		return errors.New("missing OpenIM message acknowledgement")
	}
	return err
}
