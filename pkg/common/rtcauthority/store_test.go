package rtcauthority

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
)

// storeRedis is a small RESP2 peer for exercising Store.Update without a
// separately installed Redis process. It records the writes made inside EXEC.
type storeRedis struct {
	ln       net.Listener
	mu       sync.Mutex
	data     string
	commands []string
}

func newStoreRedis(t *testing.T, data string) (*storeRedis, *redis.Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &storeRedis{ln: ln, data: data}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	c := redis.NewClient(&redis.Options{Addr: ln.Addr().String(), Protocol: 2})
	t.Cleanup(func() { _ = c.Close() })
	return s, c
}

func (s *storeRedis) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serveConn(conn)
	}
}

func readRESP(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) == 0 || line[0] != '*' {
		return nil, fmt.Errorf("expected array, got %q", line)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		line, err = r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if len(line) == 0 || line[0] != '$' {
			return nil, fmt.Errorf("expected bulk, got %q", line)
		}
		l, e := strconv.Atoi(strings.TrimSpace(line[1:]))
		if e != nil {
			return nil, e
		}
		b := make([]byte, l+2)
		if _, err = r.Read(b); err != nil {
			return nil, err
		}
		args[i] = string(b[:l])
	}
	return args, nil
}

func (s *storeRedis) serveConn(conn net.Conn) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	inMulti := false
	queued := [][]string{}
	for {
		args, err := readRESP(r)
		if err != nil {
			return
		}
		cmd := strings.ToUpper(args[0])
		s.mu.Lock()
		s.commands = append(s.commands, strings.Join(args, " "))
		s.mu.Unlock()
		switch cmd {
		case "CLIENT":
			_, _ = w.WriteString("+OK\r\n")
		case "WATCH", "UNWATCH":
			_, _ = w.WriteString("+OK\r\n")
		case "GET":
			if inMulti {
				queued = append(queued, args)
				_, _ = w.WriteString("+QUEUED\r\n")
				break
			}
			s.mu.Lock()
			data := s.data
			s.mu.Unlock()
			if data == "" {
				_, _ = w.WriteString("$-1\r\n")
			} else {
				_, _ = fmt.Fprintf(w, "$%d\r\n%s\r\n", len(data), data)
			}
		case "MULTI":
			inMulti = true
			queued = nil
			_, _ = w.WriteString("+OK\r\n")
		case "SET", "EXPIRE":
			if inMulti {
				queued = append(queued, args)
				_, _ = w.WriteString("+QUEUED\r\n")
			} else {
				_, _ = w.WriteString("+OK\r\n")
			}
		case "EXEC":
			inMulti = false
			s.mu.Lock()
			for _, q := range queued {
				if strings.EqualFold(q[0], "SET") && len(q) >= 3 {
					s.data = q[2]
				}
			}
			s.mu.Unlock()
			_, _ = fmt.Fprintf(w, "*%d\r\n", len(queued))
			for _, q := range queued {
				switch strings.ToUpper(q[0]) {
				case "GET":
					s.mu.Lock()
					data := s.data
					s.mu.Unlock()
					if data == "" {
						_, _ = w.WriteString("$-1\r\n")
					} else {
						_, _ = fmt.Fprintf(w, "$%d\r\n%s\r\n", len(data), data)
					}
				default:
					_, _ = w.WriteString("+OK\r\n")
				}
			}
		default:
			_, _ = fmt.Fprintf(w, "-ERR unsupported command %s\r\n", cmd)
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

func TestStoreUpdateUnchangedSnapshotDoesNotSET(t *testing.T) {
	ctx := context.Background()
	state := `{"calls":{"room":{"room":"room","caller":"alice","callee":"bob","status":"accepted","created":1,"expires":2,"members":{"alice":{"identity":"a","lease":9999999999999},"bob":{"identity":"b","lease":9999999999999}}}}}`
	server, client := newStoreRedis(t, state)
	store := &Store{Redis: client, Key: "rtc:test"}
	err := store.Update(ctx, func(*State) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	expire := 0
	watch, exec := false, false
	for _, command := range server.commands {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			continue
		}
		if strings.EqualFold(fields[0], "SET") {
			t.Fatalf("unchanged snapshot issued SET: %q", command)
		}
		if strings.EqualFold(fields[0], "EXPIRE") {
			expire++
		}
		watch = watch || strings.EqualFold(fields[0], "WATCH")
		exec = exec || strings.EqualFold(fields[0], "EXEC")
	}
	if expire != 1 {
		t.Fatalf("unchanged nonempty snapshot should renew TTL once with EXPIRE; commands=%q", server.commands)
	}
	if !watch || !exec {
		t.Fatalf("unchanged update must validate its WATCH transaction; commands=%q", server.commands)
	}
}

func TestStoreUpdateUnchangedEmptySnapshotDoesNotSET(t *testing.T) {
	server, client := newStoreRedis(t, `{"calls":{}}`)
	err := (&Store{Redis: client, Key: "rtc:test"}).Update(context.Background(), func(*State) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	watch, exec := false, false
	for _, command := range server.commands {
		fields := strings.Fields(command)
		if len(fields) > 0 && strings.EqualFold(fields[0], "SET") {
			t.Fatalf("unchanged empty snapshot issued SET: %q", command)
		}
		watch = watch || strings.EqualFold(fields[0], "WATCH")
		exec = exec || strings.EqualFold(fields[0], "EXEC")
	}
	if !watch || !exec {
		t.Fatalf("unchanged empty update must validate its WATCH transaction; commands=%q", server.commands)
	}
}

func TestStoreUpdateMissingKeyStillExecutesWatchedTransaction(t *testing.T) {
	server, client := newStoreRedis(t, "")
	err := (&Store{Redis: client, Key: "rtc:test"}).Update(context.Background(), func(*State) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	watch, exec := false, false
	for _, command := range server.commands {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			continue
		}
		if strings.EqualFold(fields[0], "SET") || strings.EqualFold(fields[0], "EXPIRE") {
			t.Fatalf("missing empty key should remain absent: %q", command)
		}
		watch = watch || strings.EqualFold(fields[0], "WATCH")
		exec = exec || strings.EqualFold(fields[0], "EXEC")
	}
	if !watch || !exec {
		t.Fatalf("missing key update must validate WATCH with EXEC; commands=%q", server.commands)
	}
}

func TestStoreUpdateUnchangedPendingRecordStillExecutesWatchedTransaction(t *testing.T) {
	data, err := json.Marshal(State{Calls: map[string]*Call{"room": {Room: "room", Caller: "alice", Callee: "bob", Status: "ended", Members: map[string]*Member{}, Record: &RecordDelivery{EventID: "event"}}}})
	if err != nil {
		t.Fatal(err)
	}
	server, client := newStoreRedis(t, string(data))
	err = (&Store{Redis: client, Key: "rtc:test"}).Update(context.Background(), func(*State) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	watch, exec := false, false
	for _, command := range server.commands {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			continue
		}
		switch {
		case strings.EqualFold(fields[0], "WATCH"):
			watch = true
		case strings.EqualFold(fields[0], "EXEC"):
			exec = true
		case strings.EqualFold(fields[0], "SET"), strings.EqualFold(fields[0], "EXPIRE"):
			t.Fatalf("pending unchanged state should not be rewritten or assigned a TTL: %q", command)
		}
	}
	if !watch || !exec {
		t.Fatalf("pending unchanged update must validate WATCH with EXEC; commands=%q", server.commands)
	}
}

func TestStoreViewDoesNotWrite(t *testing.T) {
	server, client := newStoreRedis(t, `{"calls":{}}`)
	err := (&Store{Redis: client, Key: "rtc:test"}).View(context.Background(), func(state *State) error {
		if len(state.Calls) != 0 {
			t.Fatalf("unexpected calls: %+v", state.Calls)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, command := range server.commands {
		fields := strings.Fields(command)
		if len(fields) > 0 && (strings.EqualFold(fields[0], "SET") || strings.EqualFold(fields[0], "EXPIRE")) {
			t.Fatalf("View issued a write: %q", command)
		}
	}
}

func TestStoreUpdatePendingRecordUsesPersistentSET(t *testing.T) {
	server, client := newStoreRedis(t, `{"calls":{}}`)
	err := (&Store{Redis: client, Key: "rtc:test"}).Update(context.Background(), func(state *State) error {
		state.Calls["room"] = &Call{Room: "room", Status: "ended", Record: &RecordDelivery{EventID: "event"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	sets := 0
	for _, command := range server.commands {
		fields := strings.Fields(command)
		if len(fields) > 0 && strings.EqualFold(fields[0], "SET") {
			sets++
			if len(fields) != 3 {
				t.Fatalf("pending record SET must be persistent (without EX): %q", command)
			}
		}
	}
	if sets != 1 {
		t.Fatalf("pending record should be persisted once, SET count=%d commands=%q", sets, server.commands)
	}
}

func TestStoreUpdateMutationUsesSET(t *testing.T) {
	server, client := newStoreRedis(t, `{"calls":{}}`)
	err := (&Store{Redis: client, Key: "rtc:test"}).Update(context.Background(), func(state *State) error {
		state.Calls["room"] = &Call{Room: "room", Status: "ringing"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	sets := 0
	for _, command := range server.commands {
		fields := strings.Fields(command)
		if len(fields) > 0 && strings.EqualFold(fields[0], "SET") {
			sets++
		}
	}
	if sets != 1 {
		t.Fatalf("mutating update should use SET once, got %d; commands=%q", sets, server.commands)
	}
}
