package callprotocol

import (
	"strings"
	"testing"
)

func TestWireCompatibility(t *testing.T) {
	for _, wire := range []string{`[OpenIM 呼叫] {"room":"r","kind":"end","expires":1}`, `[OpenIM 呼叫] {"room":"r","kind":"busy","name":"旧称呼","expires":1}`, `[OpenIM 呼叫] {"version":2,"eventID":"e","room":"r","kind":"missed","expires":1,"durationSeconds":0}`} {
		value, err := Decode(wire)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := Encode(value)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Decode(encoded)
		if err != nil || again.Room != "r" || again.Kind != value.Kind || again.EventID != value.EventID {
			t.Fatal("wire incompatible")
		}
	}
	for _, wire := range []string{"ordinary", `[OpenIM 呼叫] {}`, `[OpenIM 呼叫] {"version":3,"room":"r","kind":"end"}`, `[OpenIM 呼叫] {"room":"r","kind":"end","durationSeconds":-1}`} {
		if _, err := Decode(wire); err == nil {
			t.Fatal("invalid accepted", wire)
		}
	}
}

func TestLegacyRecordDefaultVersionStaysOmitted(t *testing.T) {
	legacy := `[OpenIM 呼叫] {"room":"r","kind":"end","expires":1}`
	record, err := Decode(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if record.Version != 0 {
		t.Fatalf("legacy default version=%d, want 0", record.Version)
	}
	encoded, err := Encode(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, `"version":0`) {
		t.Fatalf("legacy record acquired explicit version 0: %s", encoded)
	}
	if _, err = Decode(encoded); err != nil {
		t.Fatalf("legacy roundtrip no longer decodes: %v", err)
	}
}
