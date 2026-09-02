package main

// Events replay and network lifecycle against the fake containerd. The
// events ring is seeded directly (it is the same in-memory log the recorder
// fills in production); the live subscription is served by the fake's
// open-but-silent events stream.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func seedRing(events ...dockerEvent) {
	for _, ev := range events {
		eventLogRecord(ev)
	}
}

func ringEvent(action, name string, at time.Time) dockerEvent {
	return dockerEvent{
		Type:   "container",
		Action: action,
		Actor: dockerEventActor{
			ID:         dockerID("default", "seed-"+name),
			Attributes: map[string]string{"name": name, "image": "docker.io/library/alpine:latest"},
		},
		Scope:    "local",
		Time:     at.Unix(),
		TimeNano: at.UnixNano(),
	}
}

// getEventsStream fetches /events and returns the decoded events once the
// stream closes (until in the near future bounds it).
func getEventsStream(t *testing.T, srvURL, query string) []dockerEvent {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(srvURL + "/events" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var events []dockerEvent
	dec := json.NewDecoder(resp.Body)
	for {
		var ev dockerEvent
		if err := dec.Decode(&ev); err != nil {
			break
		}
		events = append(events, ev)
	}
	return events
}

func TestEventsReplaySince(t *testing.T) {
	startFakeContainerd(t, "default") // events stream only; nothing live
	srv := newTestAPIServer(t)

	now := time.Now()
	seedRing(
		ringEvent("create", "old", now.Add(-2*time.Minute)),
		ringEvent("create", "recent", now.Add(-5*time.Second)),
		ringEvent("start", "recent", now.Add(-4*time.Second)),
		ringEvent("die", "recent", now.Add(-3*time.Second)),
	)
	t.Cleanup(func() { eventLog.ring = nil })

	since := fmt.Sprintf("&since=%d", now.Add(-30*time.Second).Unix())
	until := fmt.Sprintf("&until=%d", now.Add(time.Second).Unix())
	events := getEventsStream(t, srv.URL, "?"+since[1:]+until)
	if len(events) != 3 {
		t.Fatalf("replayed %d events, want 3 (recent create/start/die): %+v", len(events), events)
	}
	want := []string{"create", "start", "die"}
	for i, ev := range events {
		if ev.Action != want[i] || ev.Actor.Attributes["name"] != "recent" {
			t.Errorf("event[%d] = %s/%s, want %s/recent", i, ev.Action, ev.Actor.Attributes["name"], want[i])
		}
	}
}

func TestEventsContainerFilterOnReplay(t *testing.T) {
	startFakeContainerd(t, "default")
	srv := newTestAPIServer(t)

	now := time.Now()
	seedRing(
		ringEvent("create", "web", now.Add(-5*time.Second)),
		ringEvent("create", "db", now.Add(-4*time.Second)),
		ringEvent("die", "web", now.Add(-3*time.Second)),
	)
	t.Cleanup(func() { eventLog.ring = nil })

	q := fmt.Sprintf("?since=%d&until=%d&filters=%%7B%%22container%%22%%3A%%7B%%22web%%22%%3Atrue%%7D%%7D",
		now.Add(-30*time.Second).Unix(), now.Add(time.Second).Unix())
	events := getEventsStream(t, srv.URL, q)
	if len(events) != 2 {
		t.Fatalf("filtered replay returned %d events, want 2: %+v", len(events), events)
	}
	for _, ev := range events {
		if ev.Actor.Attributes["name"] != "web" {
			t.Errorf("filter leaked event for %q", ev.Actor.Attributes["name"])
		}
	}
}

func TestEventsUntilInPastIsEmptyStream(t *testing.T) {
	startFakeContainerd(t, "default")
	srv := newTestAPIServer(t)

	now := time.Now()
	seedRing(ringEvent("create", "stale", now.Add(-time.Minute)))
	t.Cleanup(func() { eventLog.ring = nil })

	q := fmt.Sprintf("?since=%d&until=%d", now.Add(-2*time.Minute).Unix(), now.Add(-30*time.Second).Unix())
	events := getEventsStream(t, srv.URL, q)
	if len(events) != 0 {
		t.Fatalf("until-in-past stream returned %d events, want 0", len(events))
	}
}

// overrideNetworkDirs points the CNI config dir and the runtime-artifact dir
// at throwaway directories so network lifecycle tests touch no system paths.
func overrideNetworkDirs(t *testing.T) (cniDir, runDir string) {
	t.Helper()
	cniDir = t.TempDir()
	runDir = t.TempDir()
	oldCNI, oldRun := cniConfDir, anvilRunDir
	cniConfDir, anvilRunDir = cniDir, runDir
	t.Cleanup(func() { cniConfDir, anvilRunDir = oldCNI, oldRun })
	return cniDir, runDir
}

func TestNetworkLifecycleAgainstDirs(t *testing.T) {
	startFakeContainerd(t, "default") // remove needs the namespaces list
	cniDir, _ := overrideNetworkDirs(t)
	srv := newTestAPIServer(t)
	client := &http.Client{Timeout: 10 * time.Second}

	// Create.
	body := `{"Name":"proj1_default","CheckDuplicate":false,"Driver":"bridge",` +
		`"Labels":{"com.docker.compose.project":"proj1"}}`
	resp, err := client.Post(srv.URL+"/networks/create", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Id string `json:"Id"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || created.Id == "" {
		t.Fatalf("create: status=%d id=%q", resp.StatusCode, created.Id)
	}

	// The CNI conflist is the network — it must exist on disk.
	if entries, _ := os.ReadDir(cniDir); len(entries) == 0 {
		t.Fatalf("no conflist written to %s", cniDir)
	}

	// List shows it.
	resp, err = client.Get(srv.URL + "/networks")
	if err != nil {
		t.Fatal(err)
	}
	var networks []dockerNetwork
	json.NewDecoder(resp.Body).Decode(&networks)
	resp.Body.Close()
	found := false
	for _, nw := range networks {
		if nw.Name == "proj1_default" {
			found = true
			if nw.Labels["com.docker.compose.project"] != "proj1" {
				t.Errorf("network labels = %v", nw.Labels)
			}
		}
	}
	if !found {
		t.Fatalf("created network not listed: %+v", networks)
	}

	// Inspect by name.
	resp, err = client.Get(srv.URL + "/networks/proj1_default")
	if err != nil {
		t.Fatal(err)
	}
	var nw dockerNetwork
	json.NewDecoder(resp.Body).Decode(&nw)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || nw.Name != "proj1_default" {
		t.Fatalf("inspect: status=%d name=%q", resp.StatusCode, nw.Name)
	}

	// Unknown network is a 404 in the JSON error shape.
	resp, err = client.Get(srv.URL + "/networks/nosuchnet")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown network status = %d, want 404", resp.StatusCode)
	}

	// Delete by created ID (remove resolves ID -> name first).
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/networks/"+created.Id, nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status = %d", resp.StatusCode)
	}
	if entries, _ := os.ReadDir(cniDir); len(entries) != 0 {
		t.Errorf("conflist not removed: %d entries left", len(entries))
	}
}

func TestRenameContainerAgainstFakeContainerd(t *testing.T) {
	startFakeContainerd(t, "default", fixtureNS()...)
	srv := newTestAPIServer(t)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/containers/web/rename?name=web2", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rename status = %d, want 204", resp.StatusCode)
	}

	// The rename took effect in the store and the container is now
	// inspectable under the new name.
	resp2, err := http.Get(srv.URL + "/containers/web2/json")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("inspect as web2 status = %d", resp2.StatusCode)
	}
	resp3, err := http.Get(srv.URL + "/containers/web/json")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("inspect as old name web status = %d, want 404", resp3.StatusCode)
	}
}
