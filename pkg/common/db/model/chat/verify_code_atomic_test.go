package chat

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/openimsdk/chat/pkg/common/db/table/chat"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// These integration tests use only an explicitly supplied loopback Mongo URI and
// a unique disposable database. They never connect to a deployment database.
func otpTestDB(t *testing.T) (*VerifyCode, chat.VerifyCodeSendLimiterInterface) {
	t.Helper()
	uri := os.Getenv("OTP_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("OTP_TEST_MONGO_URI is not set")
	}
	u, err := url.Parse(uri)
	if err != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
		t.Fatal("OTP_TEST_MONGO_URI must target loopback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("otp_test_%d", time.Now().UnixNano())
	db := client.Database(dbName)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = db.Drop(cleanupCtx)
		_ = client.Disconnect(cleanupCtx)
	})
	model, err := NewVerifyCode(db)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := NewVerifyCodeSendLimiter(db)
	if err != nil {
		t.Fatal(err)
	}
	return model.(*VerifyCode), limiter
}

func TestVerifyCodeAttemptLimitIsAtomic(t *testing.T) {
	model, _ := otpTestDB(t)
	ctx := context.Background()
	vc := &chat.VerifyCode{Account: "isolated", Code: "123456", CreateTime: time.Now(), Duration: 300}
	if err := model.Add(ctx, []*chat.VerifyCode{vc}); err != nil {
		t.Fatal(err)
	}
	const limit, workers = 5, 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := model.TryIncrementAttempt(ctx, vc.ID, limit)
			if err != nil {
				t.Errorf("TryIncrementAttempt: %v", err)
				return
			}
			if ok {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != limit {
		t.Fatalf("accepted %d wrong attempts, want %d", successes, limit)
	}
	ok, err := model.TryAcceptCode(ctx, vc.ID, "123456", limit, "accept-nonce")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("correct code accepted after wrong-attempt limit was exhausted")
	}
}

func TestVerifyCodeSendReservationIsAtomicRollingAndReleasable(t *testing.T) {
	_, limiter := otpTestDB(t)
	ctx := context.Background()
	now := time.Now()
	const max = 3
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := make([]string, 0, max)
	for i := 0; i < 24; i++ {
		id := fmt.Sprintf("r-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := limiter.Reserve(ctx, "same-account", now, time.Minute, max, id)
			if err != nil {
				t.Errorf("Reserve: %v", err)
				return
			}
			if ok {
				mu.Lock()
				accepted = append(accepted, id)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(accepted) != max {
		t.Fatalf("accepted %d reservations, want %d", len(accepted), max)
	}
	if err := limiter.Release(ctx, "same-account", accepted[0]); err != nil {
		t.Fatal(err)
	}
	if ok, err := limiter.Reserve(ctx, "same-account", now, time.Minute, max, "retry-after-failed-send"); err != nil || !ok {
		t.Fatalf("released slot not reusable: ok=%v err=%v", ok, err)
	}
	if ok, err := limiter.Reserve(ctx, "same-account", now.Add(2*time.Minute), time.Minute, max, "after-window"); err != nil || !ok {
		t.Fatalf("expired rolling-window entries not pruned: ok=%v err=%v", ok, err)
	}
}
