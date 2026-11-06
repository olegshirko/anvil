package main

// Validation that must answer before any containerd call: a bad -t or an
// impossible --hostname is a 400 like dockerd, not a silently-ignored
// parameter or a start-time runc failure.

import (
	"net/http"
	"strings"
	"testing"
)

func TestStopRejectsInvalidTimeout(t *testing.T) {
	srv := newTestAPIServer(t)
	defer srv.Close()

	for _, q := range []string{"?t=abc", "?t=1.5", "?t=" /* present but empty handled as absent: skip */, "?t=0x10"} {
		if q == "?t=" {
			continue
		}
		resp, err := http.Post(srv.URL+"/containers/nope/stop"+q, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("stop %s: status %d, want 400", q, resp.StatusCode)
		}
	}
	// A valid timeout reaches the containerd path: not a 400 (it fails later
	// with 404/500 for the unknown container, which is fine here).
	resp, err := http.Post(srv.URL+"/containers/nope/stop?t=3", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		t.Error("stop ?t=3: valid timeout must not be rejected as invalid")
	}
}

func TestRestartRejectsInvalidTimeout(t *testing.T) {
	srv := newTestAPIServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/containers/nope/restart?t=nope", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("restart with bad -t: status %d, want 400", resp.StatusCode)
	}
}

func TestCreateRejectsOverlongHostname(t *testing.T) {
	srv := newTestAPIServer(t)
	defer srv.Close()

	body := `{"Image":"alpine","Hostname":"` + strings.Repeat("h", 65) + `"}`
	resp, err := http.Post(srv.URL+"/containers/create?name=web", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create with 65-char hostname: status %d, want 400", resp.StatusCode)
	}

	body = `{"Image":"alpine","Hostname":"` + strings.Repeat("h", 64) + `"}`
	resp, err = http.Post(srv.URL+"/containers/create?name=web", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		t.Error("create with 64-char hostname must not be rejected as invalid")
	}
}
