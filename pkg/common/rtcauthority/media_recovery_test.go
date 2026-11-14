package rtcauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	recoveryTestKey    = "rtc-recovery-test-key"
	recoveryTestSecret = "rtc-recovery-test-secret-only-for-tests"
)

type memoryStateRepository struct {
	mu    sync.Mutex
	state *State
}

func newMemoryStateRepository(state State) *memoryStateRepository {
	return &memoryStateRepository{state: cloneAuthorityState(&state)}
}

func cloneAuthorityState(state *State) *State {
	data, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	var cloned State
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}
	if cloned.Calls == nil {
		cloned.Calls = map[string]*Call{}
	}
	return &cloned
}

func (r *memoryStateRepository) Update(ctx context.Context, apply func(*State) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	working := cloneAuthorityState(r.state)
	if err := apply(working); err != nil {
		return err
	}
	r.state = cloneAuthorityState(working)
	return nil
}

func (r *memoryStateRepository) snapshot() *State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneAuthorityState(r.state)
}

func (r *memoryStateRepository) call(room string) *Call {
	return r.snapshot().Calls[room]
}

func newRecoveryService(repo StateRepository, internalURL string) *Service {
	return &Service{
		Store:       repo,
		Key:         recoveryTestKey,
		Secret:      recoveryTestSecret,
		InternalURL: internalURL,
		GroupMember: func(context.Context, string, string) error { return nil },
		UserExists:  func(context.Context, string) error { return nil },
	}
}

func newGroupState(t *testing.T, room string) (State, string) {
	t.Helper()
	state := State{Calls: map[string]*Call{}}
	now := time.Now().UnixMilli()
	call, err := state.Create(room, "owner", "", "group-test", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Action(room, "member", "join", now+1); err != nil {
		t.Fatal(err)
	}
	return state, call.Members["member"].Identity
}

func TestRemoveParticipantFailureKeepsQueueUntilRetrySucceeds(t *testing.T) {
	state, identity := newGroupState(t, "group-remove-retry")
	repo := newMemoryStateRepository(state)
	var requests atomic.Int32
	mediaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/RemoveParticipant") {
			t.Errorf("unexpected media method path %q", r.URL.Path)
		}
		var body struct {
			Room     string `json:"room"`
			Identity string `json:"identity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode media request: %v", err)
		}
		if body.Identity != identity {
			t.Errorf("RemoveParticipant identity = %q, want old admission identity", body.Identity)
		}
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mediaServer.Close()
	service := newRecoveryService(repo, mediaServer.URL)
	ctx := context.Background()

	if _, err := service.Execute(ctx, "member", Request{Room: "group-remove-retry", Action: "leave"}); err != nil {
		t.Fatal(err)
	}
	if !repo.call("group-remove-retry").Removals[identity] {
		t.Fatal("failed RemoveParticipant request cleared its retry entry")
	}
	if _, err := service.Execute(ctx, "owner", Request{Action: "list"}); err != nil {
		t.Fatal(err)
	}
	if repo.call("group-remove-retry").Removals[identity] {
		t.Fatal("successful RemoveParticipant retry left its queue entry")
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("RemoveParticipant requests = %d, want first failure and one retry", got)
	}
}

func TestConcurrentRemovalRetryCannotRemoveRejoinedMemberIdentity(t *testing.T) {
	const room = "group-remove-rejoin-race"
	state, oldIdentity := newGroupState(t, room)
	repo := newMemoryStateRepository(state)
	firstRequestStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseFirst) })
	var requestCount atomic.Int32
	var identitiesMu sync.Mutex
	var identities []string

	mediaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Identity string `json:"identity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode media request: %v", err)
		}
		identitiesMu.Lock()
		identities = append(identities, body.Identity)
		identitiesMu.Unlock()
		if requestCount.Add(1) == 1 {
			close(firstRequestStarted)
			<-releaseFirst
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mediaServer.Close()
	service := newRecoveryService(repo, mediaServer.URL)
	ctx := context.Background()

	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Execute(ctx, "member", Request{Room: room, Action: "leave"})
		firstDone <- err
	}()
	select {
	case <-firstRequestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first RemoveParticipant request did not start")
	}

	service.cleanupRemovals(ctx, repo.call(room))
	if repo.call(room).Removals[oldIdentity] {
		t.Fatal("second successful cleanup did not clear the old identity")
	}
	if _, err := service.Execute(ctx, "member", Request{Room: room, Action: "join"}); err != nil {
		t.Fatal(err)
	}
	newIdentity := repo.call(room).Members["member"].Identity
	if newIdentity == "" || newIdentity == oldIdentity {
		t.Fatal("rejoin reused the old media identity")
	}

	releaseOnce.Do(func() { close(releaseFirst) })
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("delayed leave request: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delayed RemoveParticipant request did not finish")
	}

	current := repo.call(room)
	if current.Members["member"] == nil || current.Members["member"].Identity != newIdentity {
		t.Fatal("late cleanup removed or replaced the rejoined member")
	}
	if current.Removals[oldIdentity] {
		t.Fatal("old identity removal was requeued after late success")
	}
	identitiesMu.Lock()
	defer identitiesMu.Unlock()
	if len(identities) != 2 || identities[0] != oldIdentity || identities[1] != oldIdentity {
		t.Fatalf("cleanup identities = %v, want exactly two requests for old identity %q", identities, oldIdentity)
	}
}

func signedWebhookRequest(t *testing.T, event *livekit.WebhookEvent) *http.Request {
	t.Helper()
	body, err := protojson.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	token, err := auth.NewAccessToken(recoveryTestKey, recoveryTestSecret).
		SetSha256(base64.StdEncoding.EncodeToString(digest[:])).
		SetValidFor(time.Minute).
		ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	r.Header.Set("Authorization", token)
	return r
}

func TestStaleParticipantLeftWebhookBeforeJoinIsIgnored(t *testing.T) {
	state, identity := newGroupState(t, "group-stale-left")
	member := state.Calls["group-stale-left"].Members["member"]
	member.SID = "PA_current-session"
	member.JoinedAt = 200
	repo := newMemoryStateRepository(state)
	service := newRecoveryService(repo, "")
	event := &livekit.WebhookEvent{
		Event: "participant_left",
		Room:  &livekit.Room{Name: "group-stale-left"},
		Participant: &livekit.ParticipantInfo{
			Identity: identity,
			Sid:      "PA_current-session",
		},
		CreatedAt: member.JoinedAt - 1,
	}
	request := signedWebhookRequest(t, event)
	response := httptest.NewRecorder()

	service.Webhook(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", response.Code)
	}
	current := repo.call("group-stale-left").Members["member"]
	if current == nil || current.LeftAt != 0 || current.SID != "PA_current-session" || current.JoinedAt != 200 {
		t.Fatalf("stale left event changed current admission: %+v", current)
	}
}

func TestCallIsNotPrunedAfterDeleteRoomFailure(t *testing.T) {
	state := State{Calls: map[string]*Call{}}
	now := time.Now().UnixMilli()
	call, err := state.Create("direct-delete-failure", "caller", "callee", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Action(call.Room, "callee", "accept", now+1); err != nil {
		t.Fatal(err)
	}
	repo := newMemoryStateRepository(state)
	var deleteRequests atomic.Int32
	mediaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/DeleteRoom") {
			t.Errorf("unexpected media method path %q", r.URL.Path)
		}
		deleteRequests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer mediaServer.Close()
	service := newRecoveryService(repo, mediaServer.URL)
	ctx := context.Background()

	if _, err := service.Execute(ctx, "caller", Request{Room: call.Room, Action: "end"}); err != nil {
		t.Fatal(err)
	}
	ended := repo.call(call.Room)
	if !ended.Terminal() || ended.MediaClosed {
		t.Fatalf("failed DeleteRoom state = terminal:%v mediaClosed:%v", ended.Terminal(), ended.MediaClosed)
	}
	if got := deleteRequests.Load(); got != 1 {
		t.Fatalf("DeleteRoom requests = %d, want 1", got)
	}

	pruneAt := ended.Ended + 24*60*60*1000 + 1
	if err := repo.Update(ctx, func(state *State) error {
		state.Sweep(pruneAt)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if repo.call(call.Room) == nil {
		t.Fatal(fmt.Sprintf("call %q was pruned before media room closure completed", call.Room))
	}
}
