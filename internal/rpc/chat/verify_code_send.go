package chat

import (
	"context"
	"errors"
	"time"

	chatdb "github.com/openimsdk/chat/pkg/common/db/table/chat"
)

type verifyCodeSendStore interface {
	ReserveVerifyCodeSend(context.Context, string, time.Time, time.Duration, int, string) (bool, error)
	ReleaseVerifyCodeSend(context.Context, string, string) error
	AddVerifyCode(context.Context, *chatdb.VerifyCode, func() error) error
}

func reserveAndSendVerifyCode(ctx context.Context, store verifyCodeSendStore, vc *chatdb.VerifyCode, window time.Duration, max int, reservationID string, send func() error) (bool, error) {
	reserved, err := store.ReserveVerifyCodeSend(ctx, vc.Account, vc.CreateTime, window, max, reservationID)
	if err != nil || !reserved {
		return reserved, err
	}
	if err := store.AddVerifyCode(ctx, vc, send); err != nil {
		if releaseErr := store.ReleaseVerifyCodeSend(ctx, vc.Account, reservationID); releaseErr != nil {
			return true, errors.Join(err, releaseErr)
		}
		return true, err
	}
	return true, nil
}
