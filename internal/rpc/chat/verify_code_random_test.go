package chat

import (
	"testing"
)

func TestRandomNumericCode(t *testing.T) {
	for _, length := range []int{1, 4, 6, 12} {
		code, err := randomNumericCode(length)
		if err != nil {
			t.Fatalf("randomNumericCode(%d): %v", length, err)
		}
		if len(code) != length {
			t.Fatalf("got length %d, want %d", len(code), length)
		}
		for _, digit := range code {
			if digit < '0' || digit > '9' {
				t.Fatalf("non-digit in generated code")
			}
		}
	}
	for _, length := range []int{0, -1, 1000001} {
		if _, err := randomNumericCode(length); err == nil {
			t.Fatalf("randomNumericCode(%d) unexpectedly succeeded", length)
		}
	}
}
