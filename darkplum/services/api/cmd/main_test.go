package main

import (
	"testing"
	"time"
)

func TestParseModelSyncInterval(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"empty uses default", "", 60 * time.Second},
		{"valid value passes through", "30", 30 * time.Second},
		{"non-numeric falls back to default", "not-a-number", 60 * time.Second},
		{"below the floor is clamped up", "5", 10 * time.Second},
		{"zero is clamped up", "0", 10 * time.Second},
		{"negative is clamped up", "-5", 10 * time.Second},
		{"exactly the floor is accepted", "10", 10 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseModelSyncInterval(tc.raw); got != tc.want {
				t.Errorf("parseModelSyncInterval(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
