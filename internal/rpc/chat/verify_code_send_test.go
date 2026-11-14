package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	chatdb "github.com/openimsdk/chat/pkg/common/db/table/chat"
)

type verifyCodeSendStoreFake struct {
	reserved bool
	released bool
	addErr   error
}

func (f *verifyCodeSendStoreFake) ReserveVerifyCodeSend(context.Context, string, time.Time, time.Duration, int, string) (bool, error) {
	return f.reserved, nil
}
func (f *verifyCodeSendStoreFake) ReleaseVerifyCodeSend(context.Context, string, string) error {
	f.released = true
	return nil
}
func (f *verifyCodeSendStoreFake) AddVerifyCode(_ context.Context, _ *chatdb.VerifyCode, send func() error) error {
	if f.addErr != nil {
		return f.addErr
	}
	return send()
}

func TestReserveAndSendVerifyCodeReleasesFailedSendOnly(t *testing.T) {
	ctx := context.Background()
	vc := &chatdb.VerifyCode{Account: "test-only", CreateTime: time.Now()}
	sendFailure := errors.New("test provider failure")
	store := &verifyCodeSendStoreFake{reserved: true}
	ok, err := reserveAndSendVerifyCode(ctx, store, vc, time.Minute, 2, "failed", func() error { return sendFailure })
	if !ok || !errors.Is(err, sendFailure) || !store.released {
		t.Fatalf("failed send did not release reservation: reserved=%v err=%v released=%v", ok, err, store.released)
	}
	store = &verifyCodeSendStoreFake{reserved: true}
	ok, err = reserveAndSendVerifyCode(ctx, store, vc, time.Minute, 2, "success", func() error { return nil })
	if !ok || err != nil || store.released {
		t.Fatalf("successful send should retain reservation: reserved=%v err=%v released=%v", ok, err, store.released)
	}
}

func TestReserveAndSendVerifyCodeDoesNotSendWhenLimitDenied(t *testing.T) {
	store := &verifyCodeSendStoreFake{reserved: false}
	sent := false
	ok, err := reserveAndSendVerifyCode(context.Background(), store, &chatdb.VerifyCode{Account: "test-only", CreateTime: time.Now()}, time.Minute, 1, "denied", func() error { sent = true; return nil })
	if ok || err != nil || sent || store.released {
		t.Fatalf("denied reservation should not send: reserved=%v err=%v sent=%v", ok, err, sent)
	}
}
