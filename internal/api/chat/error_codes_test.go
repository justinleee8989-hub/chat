package chat

import (
	"errors"
	"strings"
	"testing"

	"github.com/openimsdk/chat/pkg/common/messageextras"
	"github.com/openimsdk/chat/pkg/common/rtcauthority"
	"github.com/openimsdk/chat/pkg/eerrs"
	"github.com/openimsdk/tools/errs"
)

func codeOf(t *testing.T, err error) (int, string) {
	t.Helper()
	var code errs.CodeError
	if !errors.As(err, &code) {
		t.Fatalf("%v is not a code error", err)
	}
	return code.Code(), code.Detail()
}

func TestFailuresAreAnsweredByCodeWithoutUpstreamDetail(t *testing.T) {
	upstream := errors.New("upstream private error detail")
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"call permission", rtcError(rtcauthority.ErrPermission), errs.ErrNoPermission.Code()},
		{"callee busy", rtcError(rtcauthority.ErrBusy), eerrs.ErrCallBusy.Code()},
		{"call ended", rtcError(rtcauthority.ErrState), eerrs.ErrCallEnded.Code()},
		{"call store busy", rtcError(rtcauthority.ErrStoreBusy), eerrs.ErrRetryLater.Code()},
		{"call request id", rtcError(rtcauthority.ErrRequestID), errs.ErrArgs.Code()},
		{"call upstream", rtcError(upstream), errs.ErrInternalServer.Code()},
		{"extras conflict", extrasError(messageextras.ErrConflict), eerrs.ErrMessageConflict.Code()},
		{"extras invalid", extrasError(messageextras.ErrInvalid), errs.ErrArgs.Code()},
		{"extras disabled", extrasError(errExtrasDisabled), eerrs.ErrFeatureDisabled.Code()},
		{"extras not synced", extrasError(errMessageNotSynced), eerrs.ErrRetryLater.Code()},
		{"extras upstream", extrasError(upstream), errs.ErrInternalServer.Code()},
		{"readers limit", readersError(errReaderLimit), eerrs.ErrReadReceiptLimit.Code()},
		{"readers upstream", readersError(upstream), eerrs.ErrRetryLater.Code()},
	} {
		got, detail := codeOf(t, tc.err)
		if got != tc.want {
			t.Fatalf("%s: code %d, want %d", tc.name, got, tc.want)
		}
		if strings.Contains(detail, "upstream private") {
			t.Fatalf("%s: upstream detail reached the client: %q", tc.name, detail)
		}
	}
}

func TestACodeErrorPassesThrough(t *testing.T) {
	if got, _ := codeOf(t, extrasError(eerrs.ErrRefuseFriend)); got != eerrs.ErrRefuseFriend.Code() {
		t.Fatalf("code %d, want %d", got, eerrs.ErrRefuseFriend.Code())
	}
}
