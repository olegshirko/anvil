package main

import (
	"encoding/json"
	"testing"
	"time"
)

func sampleEvent() dockerEvent {
	return dockerEvent{
		Type:   "container",
		Action: "die",
		Actor: dockerEventActor{
			ID: "abcdef1234567890",
			Attributes: map[string]string{
				"name":                       "web-1",
				"image":                      "docker.io/library/nginx:alpine",
				"com.docker.compose.service": "web",
				"exitCode":                   "0",
			},
		},
	}
}

func TestParseEventFilters(t *testing.T) {
	// Both wire forms: list values and the CLI's map[value]bool.
	listForm, _ := json.Marshal(map[string][]string{
		"type":      {"container"},
		"event":     {"die", "stop"},
		"container": {"web-1"},
		"label":     {"com.docker.compose.service=web"},
		"unknown":   {"whatever"},
	})
	f, err := parseEventFilters(string(listForm))
	if err != nil {
		t.Fatalf("parse list form: %v", err)
	}
	if len(f.types) != 1 || len(f.events) != 2 || len(f.containers) != 1 || len(f.labels) != 1 {
		t.Fatalf("unexpected filter contents: %+v", f)
	}
	setForm, _ := json.Marshal(map[string]map[string]bool{
		"event": {"die": true, "start": true},
	})
	f, err = parseEventFilters(string(setForm))
	if err != nil {
		t.Fatalf("parse set form: %v", err)
	}
	if len(f.events) != 2 {
		t.Fatalf("set form events: %+v", f)
	}
	if _, err := parseEventFilters("{bad json"); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if f, err := parseEventFilters(""); err != nil || len(f.types)+len(f.events) != 0 {
		t.Fatalf("empty filters should parse to a no-op: %+v err=%v", f, err)
	}
}

func TestEventFilterMatch(t *testing.T) {
	ev := sampleEvent()
	cases := []struct {
		name   string
		filter map[string][]string
		match  bool
	}{
		{"no filters", nil, true},
		{"type ok", map[string][]string{"type": {"container"}}, true},
		{"type mismatch", map[string][]string{"type": {"network"}}, false},
		{"action ok", map[string][]string{"event": {"die"}}, true},
		{"action case-insensitive", map[string][]string{"event": {"DIE"}}, true},
		{"action mismatch", map[string][]string{"event": {"start"}}, false},
		{"container by name", map[string][]string{"container": {"web-1"}}, true},
		{"container by id prefix", map[string][]string{"container": {"abcdef"}}, true},
		{"container leading slash", map[string][]string{"container": {"/web-1"}}, true},
		{"container mismatch", map[string][]string{"container": {"db-1"}}, false},
		{"image suffix match", map[string][]string{"image": {"nginx:alpine"}}, true},
		{"image mismatch", map[string][]string{"image": {"alpine"}}, false},
		{"label key only", map[string][]string{"label": {"com.docker.compose.service"}}, true},
		{"label key=value ok", map[string][]string{"label": {"com.docker.compose.service=web"}}, true},
		{"label key=value mismatch", map[string][]string{"label": {"com.docker.compose.service=db"}}, false},
		{"label missing key", map[string][]string{"label": {"nope"}}, false},
		{"combined ok", map[string][]string{"type": {"container"}, "event": {"die"}, "container": {"web-1"}}, true},
		{"combined one fails", map[string][]string{"type": {"container"}, "event": {"start"}}, false},
	}
	for _, tc := range cases {
		raw, _ := json.Marshal(tc.filter)
		f, err := parseEventFilters(string(raw))
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		if got := f.match(ev); got != tc.match {
			t.Errorf("%s: match=%v want %v", tc.name, got, tc.match)
		}
	}
}

func TestParseEventTimestamp(t *testing.T) {
	if ts := parseEventTimestamp(""); ts != nil {
		t.Errorf("empty string should give nil, got %v", ts)
	}
	if ts := parseEventTimestamp("garbage"); ts != nil {
		t.Errorf("garbage should give nil, got %v", ts)
	}
	// Unix seconds with fraction.
	ts := parseEventTimestamp("1700000000.5")
	if ts == nil || ts.Unix() != 1700000000 {
		t.Errorf("seconds parse: %v", ts)
	}
	// Unix nanoseconds (docker --until sends nanos when absolute).
	ts = parseEventTimestamp("1700000000000000123")
	if ts == nil || ts.UnixNano() != 1700000000000000123 {
		t.Errorf("nanos parse: %v", ts)
	}
	// RFC3339.
	ts = parseEventTimestamp("2024-01-01T00:00:00Z")
	if ts == nil || !ts.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("rfc3339 parse: %v", ts)
	}
	// Relative CLI forms are resolved client-side; a past timestamp must
	// still parse to a valid past time.
	past := time.Now().Add(-time.Hour)
	if ts := parseEventTimestamp(past.Format(time.RFC3339Nano)); ts == nil || ts.After(time.Now()) {
		t.Errorf("past timestamp misparsed: %v", ts)
	}
}
