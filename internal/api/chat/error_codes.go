package chat

import (
	"errors"

	"github.com/openimsdk/chat/pkg/common/messageextras"
	"github.com/openimsdk/chat/pkg/common/rtcauthority"
	"github.com/openimsdk/chat/pkg/eerrs"
	"github.com/openimsdk/tools/errs"
)

// The API answers a failure with a code (pkg/eerrs, openimsdk errs); the app puts it into words in
// its own language. The detail is English, for logs only, and an unrecognised failure (an upstream
// service's) gets a fixed detail, so its internals never reach the client.

var (
	errExtrasDisabled         = errors.New("message extras service disabled")
	errMessageNotSynced       = errors.New("message not synced yet")
	errReadersDisabled        = errors.New("group read receipts disabled")
	errReaderLimit            = errors.New("group too large for read details")
	errMemberListIncomplete   = errors.New("group member list incomplete")
	errIndexBackfill          = errors.New("old message index still being filled")
	errReadProgressIncomplete = errors.New("read progress response incomplete")
)

// rtcError is the code for a failure of the call authority.
func rtcError(err error) error {
	var code errs.CodeError
	switch {
	case errors.Is(err, rtcauthority.ErrPermission):
		return errs.ErrNoPermission.WrapMsg(err.Error())
	case errors.Is(err, rtcauthority.ErrBusy):
		return eerrs.ErrCallBusy.WrapMsg(err.Error())
	case errors.Is(err, rtcauthority.ErrState):
		return eerrs.ErrCallEnded.WrapMsg(err.Error())
	case errors.Is(err, rtcauthority.ErrCapacity), errors.Is(err, rtcauthority.ErrStoreBusy):
		return eerrs.ErrRetryLater.WrapMsg(err.Error())
	case errors.Is(err, rtcauthority.ErrRequestID):
		return errs.ErrArgs.WrapMsg(err.Error())
	case errors.As(err, &code):
		return err
	default:
		return errs.ErrInternalServer.WrapMsg("call authority failed")
	}
}

// extrasError is the code for a failure to read or change a message's edits and reactions.
func extrasError(err error) error {
	var code errs.CodeError
	switch {
	case errors.Is(err, messageextras.ErrPermission), errors.Is(err, rtcauthority.ErrPermission):
		return errs.ErrNoPermission.WrapMsg(err.Error())
	case errors.Is(err, messageextras.ErrConflict):
		return eerrs.ErrMessageConflict.WrapMsg(err.Error())
	case errors.Is(err, messageextras.ErrInvalid):
		return errs.ErrArgs.WrapMsg(err.Error())
	case errors.Is(err, errExtrasDisabled):
		return eerrs.ErrFeatureDisabled.WrapMsg(err.Error())
	case errors.Is(err, errMessageNotSynced), errors.Is(err, errIndexBackfill):
		return eerrs.ErrRetryLater.WrapMsg(err.Error())
	case errors.As(err, &code):
		return err
	default:
		return errs.ErrInternalServer.WrapMsg("message extras failed")
	}
}

// readersError is the code for a failure to list who read a group message.
func readersError(err error) error {
	switch {
	case errors.Is(err, rtcauthority.ErrPermission):
		return errs.ErrNoPermission.WrapMsg(err.Error())
	case errors.Is(err, errReadersDisabled):
		return eerrs.ErrFeatureDisabled.WrapMsg(err.Error())
	case errors.Is(err, errReaderLimit):
		return eerrs.ErrReadReceiptLimit.WrapMsg(err.Error())
	default:
		return eerrs.ErrRetryLater.WrapMsg("read details unavailable")
	}
}
