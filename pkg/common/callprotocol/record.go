// Package callprotocol owns the versioned wire format shared by call integrations.
// The legacy prefix remains a transport identifier, not a localized UI label.
package callprotocol

import (
	"encoding/json"
	"errors"
	"strings"
)

const LegacyPrefix = "[OpenIM 呼叫] "

type Record struct {
	Name            string `json:"name,omitempty"`
	Version         int    `json:"version,omitempty"`
	EventID         string `json:"eventID"`
	Kind            string `json:"kind"`
	Room            string `json:"room"`
	Caller          string `json:"caller"`
	Callee          string `json:"callee"`
	Status          string `json:"status"`
	Expires         int64  `json:"expires"`
	Ended           int64  `json:"ended"`
	ConnectedAt     int64  `json:"connectedAt,omitempty"`
	DurationSeconds *int64 `json:"durationSeconds,omitempty"`
}

func validate(r Record) error {
	if r.Room == "" || (r.Version != 0 && r.Version != 1 && r.Version != 2) || r.Expires < 0 || r.Ended < 0 || r.ConnectedAt < 0 || (r.DurationSeconds != nil && *r.DurationSeconds < 0) {
		return errors.New("invalid call record")
	}
	switch r.Kind {
	case "busy", "invite", "accept", "reject", "cancel", "end", "missed", "disconnected":
	default:
		return errors.New("invalid call kind")
	}
	return nil
}
func Encode(r Record) (string, error) {
	if err := validate(r); err != nil {
		return "", err
	}
	b, err := json.Marshal(r)
	return LegacyPrefix + string(b), err
}
func Decode(text string) (Record, error) {
	var r Record
	if !strings.HasPrefix(text, LegacyPrefix) {
		return r, errors.New("not a call record")
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(text, LegacyPrefix)), &r); err != nil {
		return r, err
	}
	return r, validate(r)
}
