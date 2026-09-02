package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// writeJSONError must produce wire-valid JSON even when the message itself
// contains quotes — the hand-concatenation it replaced did not (a %q
// validator error like `log driver "syslog" ...` broke every client parser).
func TestWriteJSONErrorEscapesQuotes(t *testing.T) {
	cases := []string{
		`log driver "syslog" is not supported by anvil: only json-file and none exist`,
		`platform "linux/amd64" is not supported`,
		"multiline\nmessage",
	}
	for _, msg := range cases {
		rec := httptest.NewRecorder()
		writeJSONError(rec, 400, msg)
		var body struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid JSON on wire for %q: %v (body=%s)", msg, err, rec.Body.String())
		}
		if body.Message != msg {
			t.Errorf("message mangled: %q != %q", body.Message, msg)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type %q, want application/json", ct)
		}
		if rec.Code != 400 {
			t.Errorf("status %d, want 400", rec.Code)
		}
	}
}
