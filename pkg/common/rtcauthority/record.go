package rtcauthority

import (
	"context"
	"github.com/google/uuid"
	"github.com/openimsdk/chat/pkg/common/callprotocol"
	"time"
)

// RecordDelivery is committed atomically with the terminal transition. A lease
// serializes healthy workers; a crash or ambiguous send result may be retried.
// The OpenIM HTTP API generates clientMsgID, so consumers deduplicate EventID/Room.
type RecordDelivery struct {
	EventID    string `json:"eventID"`
	Delivered  bool   `json:"delivered,omitempty"`
	Owner      string `json:"owner,omitempty"`
	LeaseUntil int64  `json:"leaseUntil,omitempty"`
}

// TerminalRecord retains the public name for existing integrations.
type TerminalRecord = callprotocol.Record

func (c *Call) terminalRecord() TerminalRecord {
	kind := c.Status
	switch kind {
	case "canceled":
		kind = "cancel"
	case "rejected":
		kind = "reject"
	case "ended":
		kind = "end"
	}
	return TerminalRecord{Version: 2, EventID: c.Record.EventID, Kind: kind, Room: c.Room, Caller: c.Caller, Callee: c.Callee, Status: c.Status, Expires: c.Expires, Ended: c.Ended, ConnectedAt: c.ConnectedAt, DurationSeconds: c.DurationSeconds}
}
func (s *Service) runRecords(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.deliverRecords(ctx)
		}
	}
}

func (s *Service) deliverRecords(ctx context.Context) {
	if s.SendRecord == nil {
		return
	}
	// One claim at a time keeps leases from expiring while waiting behind sends.
	attempted := map[string]bool{}
	for ctx.Err() == nil {
		owner := uuid.NewString()
		now := time.Now().UnixMilli()
		var record *TerminalRecord
		err := s.Store.Update(ctx, func(st *State) error {
			record = nil
			for room, c := range st.Calls {
				if attempted[room] || c == nil || !c.Terminal() || c.Record == nil || c.Record.Delivered || c.Record.LeaseUntil > now {
					continue
				}
				c.Record.Owner = owner
				c.Record.LeaseUntil = now + 60000
				r := c.terminalRecord()
				record = &r
				break
			}
			return nil
		})
		if err != nil || record == nil {
			return
		}
		attempted[record.Room] = true
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		sendErr := s.SendRecord(sendCtx, *record)
		cancel()
		if err = s.Store.Update(ctx, func(st *State) error {
			c := st.Calls[record.Room]
			if c != nil && c.Record != nil && c.Record.Owner == owner {
				c.Record.Delivered = sendErr == nil
				c.Record.Owner = ""
				c.Record.LeaseUntil = 0
			}
			return nil
		}); err != nil {
			return
		}
	}
}

// Only current, leased media admissions may establish a connection. Timestamp
// order and SID matching prevent delayed/duplicate events reviving a departed SID.
func (c *Call) mediaEvent(event, identity, sid string, createdAt, now int64) {
	if c.Terminal() || sid == "" {
		return
	}
	user := c.UserForIdentity(identity)
	m := c.Members[user]
	if m == nil || !c.Allows(user, now) {
		return
	}
	if event == "participant_joined" {
		if createdAt < c.Created/1000 || createdAt > now/1000+5 || createdAt < m.JoinedAt || (m.SID != "" && createdAt == m.JoinedAt && m.LeftAt == 0) || sid == m.SID || m.RetiredSIDs[sid] {
			return
		}
		if m.LeftAt > 0 && createdAt < m.LeftAt/1000 {
			return
		}
		if m.SID != "" {
			if m.RetiredSIDs == nil {
				m.RetiredSIDs = map[string]bool{}
			}
			m.RetiredSIDs[m.SID] = true
		}
		m.SID = sid
		m.JoinedAt = createdAt
		m.LeftAt = 0
		if c.ConnectedAt == 0 {
			joined := 0
			for _, member := range c.Members {
				if member.SID != "" && member.LeftAt == 0 && member.Lease > now {
					joined++
				}
			}
			if joined >= 2 {
				c.ConnectedAt = now
			}
		}
	}
	if event == "participant_left" && m.SID == sid && createdAt >= m.JoinedAt && m.LeftAt == 0 {
		m.LeftAt = now
	}
}
