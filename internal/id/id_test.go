package id

import (
	"strings"
	"testing"
	"time"
)

func TestEncodeKnownValues(t *testing.T) {
	var zero, max [16]byte
	for i := range max {
		max[i] = 0xff
	}
	// The ULID spec gives 7ZZZZZZZZZZZZZZZZZZZZZZZZZ as the largest ULID.
	tests := []struct {
		in   [16]byte
		want string
	}{
		{zero, "00000000000000000000000000"},
		{max, "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"},
		{[16]byte{15: 1}, "00000000000000000000000001"},
		{[16]byte{15: 32}, "00000000000000000000000010"},
	}
	for _, tt := range tests {
		if got := encode(tt.in); got != tt.want {
			t.Errorf("encode(%x) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestTimestampPrefix(t *testing.T) {
	// 1469918176385 ms is the spec's example timestamp, 01ARYZ6S41.
	got := newAt(time.UnixMilli(1469918176385))
	if !strings.HasPrefix(got, "01ARYZ6S41") {
		t.Errorf("newAt = %s, want prefix 01ARYZ6S41", got)
	}
}

func TestNewSortsByTime(t *testing.T) {
	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	prev := newAt(base)
	for i := 1; i < 100; i++ {
		next := newAt(base.Add(time.Duration(i) * time.Millisecond))
		if next <= prev {
			t.Fatalf("%s sorts before %s, which is older", next, prev)
		}
		prev = next
	}
}

func TestNewIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 10000 {
		id := New()
		if len(id) != 26 || strings.Trim(id, alphabet) != "" {
			t.Fatalf("malformed id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}
