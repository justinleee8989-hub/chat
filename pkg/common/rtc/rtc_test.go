package rtc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestGetLiveKitTokenConcurrentClaims(t *testing.T) {
	const (
		key    = "rtc-test-api-key"
		secret = "rtc-test-api-secret-never-used-outside-this-test"
		url    = "wss://rtc-test.invalid"
		count  = 32
	)

	liveKit := NewLiveKit(key, secret, url)
	type result struct {
		room     string
		identity string
		token    string
		err      error
	}
	results := make(chan result, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			room := fmt.Sprintf("rtc-test-room-%02d", i)
			identity := fmt.Sprintf("rtc-test-user-%02d", i)
			token, err := liveKit.GetLiveKitToken(room, identity)
			results <- result{room: room, identity: identity, token: token, err: err}
		}()
	}
	wg.Wait()
	close(results)

	for got := range results {
		if got.err != nil {
			t.Errorf("GetLiveKitToken(%q, %q) returned an error", got.room, got.identity)
			continue
		}
		parts := strings.Split(got.token, ".")
		if len(parts) != 3 {
			t.Errorf("GetLiveKitToken(%q, %q) returned a malformed JWT", got.room, got.identity)
			continue
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Errorf("decode payload for %q: %v", got.identity, err)
			continue
		}
		var claims struct {
			Subject string `json:"sub"`
			Video   struct {
				Room string `json:"room"`
			} `json:"video"`
		}
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Errorf("decode claims for %q: %v", got.identity, err)
			continue
		}
		if claims.Subject != got.identity {
			t.Errorf("token subject mismatch: want %q, got %q", got.identity, claims.Subject)
		}
		if claims.Video.Room != got.room {
			t.Errorf("token room mismatch for %q: want %q, got %q", got.identity, got.room, claims.Video.Room)
		}
	}
}
