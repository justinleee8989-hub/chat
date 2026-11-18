package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openimsdk/chat/pkg/common/rtcauthority"
	"github.com/openimsdk/protocol/constant"
)

func TestRTCRecordUsesCallerAndOfflineText(t *testing.T) {
	r := rtcauthority.TerminalRecord{Version: 2, EventID: "rtc-terminal:r", Kind: "missed", Room: "r", Caller: "a", Callee: "b", Status: "missed", Ended: 100}
	req, err := rtcRecordRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if req.SendID != "a" || req.RecvID != "b" || req.IsOnlineOnly || req.ContentType != constant.Text || req.SessionType != constant.SingleChatType {
		t.Fatalf("invalid routing: %+v", req)
	}
	content := req.Content["content"].(string)
	if !strings.HasPrefix(content, "[OpenIM 呼叫] ") {
		t.Fatal(content)
	}
	var decoded rtcauthority.TerminalRecord
	if err = json.Unmarshal([]byte(strings.TrimPrefix(content, "[OpenIM 呼叫] ")), &decoded); err != nil || decoded.EventID != r.EventID {
		t.Fatal("invalid terminal payload")
	}
}
