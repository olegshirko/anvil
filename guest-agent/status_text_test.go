package main

import (
	"testing"
	"time"
)

func TestDockerStatusText(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		state    string
		code     int
		started  time.Time
		finished time.Time
		want     string
	}{
		{"running", 0, now.Add(-5 * time.Minute), time.Time{}, "Up 5 minutes"},
		{"running", 0, now.Add(-30 * time.Second), time.Time{}, "Up 30 seconds"},
		{"running", 0, now.Add(-70 * time.Minute), time.Time{}, "Up About an hour"},
		{"running", 0, time.Time{}, time.Time{}, "Up"},
		{"paused", 0, now.Add(-3 * time.Hour), time.Time{}, "Up 3 hours (Paused)"},
		{"exited", 137, now.Add(-time.Hour), now.Add(-2 * time.Minute), "Exited (137) 2 minutes ago"},
		{"exited", 0, time.Time{}, time.Time{}, "Exited (0)"},
		{"created", 0, time.Time{}, time.Time{}, "Created"},
	}
	for _, c := range cases {
		if got := dockerStatusText(c.state, c.code, c.started, c.finished, now); got != c.want {
			t.Errorf("%s/%d: got %q, want %q", c.state, c.code, got, c.want)
		}
	}
	if got := humanDuration(3 * 24 * time.Hour); got != "3 days" {
		t.Errorf("3 days: %q", got)
	}
}
