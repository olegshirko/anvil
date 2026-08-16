package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/typeurl/v2"
)

// Docker-compatible event. Compose subscribes to /events to track container
// lifecycle (notably the die event's exitCode for --abort-on-container-exit).
type dockerEvent struct {
	Type     string           `json:"Type"`
	Action   string           `json:"Action"`
	Actor    dockerEventActor `json:"Actor"`
	Scope    string           `json:"scope"`
	Time     int64            `json:"time"`
	TimeNano int64            `json:"timeNano"`
}

type dockerEventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes,omitempty"`
}

// handleEvents streams live containerd task events as Docker JSON events.
// Supported: the `filters` query param (type/event/container/image/label)
// and `until` (timestamp — the stream closes when reached; the CLI relies
// on this for `docker events --until +Ns` to terminate). Historical replay
// (`since` in the past) is not possible: there is no event log, so only
// live events are delivered.
func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"message":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}
	filter, err := parseEventFilters(r.URL.Query().Get("filters"))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"invalid filters: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}
	// `until` in the past → empty stream; in the future → a deadline that
	// closes the stream (docker events --until +5s).
	var untilTimer <-chan time.Time
	if until := parseEventTimestamp(r.URL.Query().Get("until")); until != nil {
		if !until.After(time.Now()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			return
		}
		timer := time.NewTimer(time.Until(*until))
		defer timer.Stop()
		untilTimer = timer.C
	}

	cl, err := client.New(containerdSocket)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer cl.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	eventCh, errCh := cl.Subscribe(ctx)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-ctx.Done():
			return
		case <-untilTimer:
			return
		case err := <-errCh:
			if err != nil {
				log.Printf("[docker-api] events stream error: %v", err)
				return
			}
		case env := <-eventCh:
			if env == nil {
				continue
			}
			ev, ok := translateDockerEvent(ctx, cl, env)
			if !ok {
				debugLog("events: skip topic=%s ns=%s", env.Topic, env.Namespace)
				continue
			}
			if !filter.match(ev) {
				debugLog("events: filtered out %s id=%s", ev.Action, ev.Actor.ID[:12])
				continue
			}
			debugLog("events: send %s id=%s attrs=%v", ev.Action, ev.Actor.ID[:12], ev.Actor.Attributes)
			if err := enc.Encode(ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// eventFilters carries the supported docker events --filter values.
type eventFilters struct {
	containers []string // container ID prefix or name
	events     []string // action name (create/start/die/...)
	types      []string // event type (container)
	images     []string // image reference substring match
	labels     []string // "key" or "key=value"
}

// parseEventFilters decodes the JSON `filters` query param. The docker CLI
// sends {"key":{"value":true}} (map[string]map[string]bool); the API docs
// also allow {"key":["value"]}. Both are accepted. Unknown keys are ignored.
func parseEventFilters(raw string) (*eventFilters, error) {
	f := &eventFilters{}
	if raw == "" {
		return f, nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, err
	}
	pick := func(key string) []string {
		rawVals, ok := decoded[key]
		if !ok {
			return nil
		}
		var list []string
		if err := json.Unmarshal(rawVals, &list); err == nil {
			return list
		}
		var set map[string]bool
		if err := json.Unmarshal(rawVals, &set); err == nil {
			for v, on := range set {
				if on {
					list = append(list, v)
				}
			}
		}
		return list
	}
	f.containers = pick("container")
	f.events = pick("event")
	f.types = pick("type")
	f.images = pick("image")
	f.labels = pick("label")
	return f, nil
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

// match reports whether the event passes every present filter.
func (f *eventFilters) match(ev dockerEvent) bool {
	if len(f.types) > 0 && !containsFold(f.types, ev.Type) {
		return false
	}
	if len(f.events) > 0 && !containsFold(f.events, ev.Action) {
		return false
	}
	if len(f.containers) > 0 {
		name := ev.Actor.Attributes["name"]
		matched := false
		for _, c := range f.containers {
			c = strings.TrimPrefix(c, "/")
			if strings.HasPrefix(ev.Actor.ID, c) || (name != "" && name == c) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(f.images) > 0 {
		image := ev.Actor.Attributes["image"]
		matched := false
		for _, want := range f.images {
			// Docker matches the full reference; images differ in
			// registry/tag normalization, so compare the base name too.
			if image == want || strings.HasSuffix(image, "/"+want) || strings.HasSuffix(want, "/"+image) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(f.labels) > 0 {
		for _, l := range f.labels {
			k, v, hasVal := strings.Cut(l, "=")
			got, ok := ev.Actor.Attributes[k]
			if !ok {
				return false
			}
			if hasVal && got != v {
				return false
			}
		}
	}
	return true
}

// parseEventTimestamp accepts the forms the docker CLI sends: unix seconds
// (float), unix nanoseconds, or RFC3339. Returns nil when absent/invalid.
func parseEventTimestamp(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	// Integer first: nanosecond timestamps exceed float64 precision.
	if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var t time.Time
		if i > 1e15 {
			t = time.Unix(0, i)
		} else {
			t = time.Unix(i, 0)
		}
		return &t
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		s, ns := math.Modf(f)
		t := time.Unix(int64(s), int64(ns*1e9))
		return &t
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return &t
	}
	return nil
}

// translateDockerEvent maps a containerd task event envelope to a Docker
// container event. The actor ID is the deterministic Docker ID so clients
// can match it against the IDs returned by the Docker API. Attributes carry
// the container's labels (plus name/image/exitCode) — Docker enriches events
// the same way, and compose's monitor matches events to services by the
// com.docker.compose.service label.
func translateDockerEvent(ctx context.Context, cl *client.Client, env *events.Envelope) (dockerEvent, bool) {
	var action string
	switch env.Topic {
	case "/tasks/create":
		action = "create"
	case "/tasks/start":
		action = "start"
	case "/tasks/exit":
		action = "die"
	case "/tasks/delete":
		action = "destroy"
	default:
		return dockerEvent{}, false
	}

	msg, err := typeurl.UnmarshalAny(env.Event)
	if err != nil {
		return dockerEvent{}, false
	}
	var containerdID, exitCode string
	switch e := msg.(type) {
	case *eventsapi.TaskCreate:
		containerdID = e.ContainerID
	case *eventsapi.TaskStart:
		containerdID = e.ContainerID
	case *eventsapi.TaskExit:
		containerdID = e.ContainerID
		exitCode = strconv.Itoa(int(e.ExitStatus))
		// containerd reports 0 for signal deaths; docker kill caches the
		// Docker-conventional 128+N code — prefer it when present.
		if e.ExitStatus == 0 {
			if code, ok := peekContainerExitCode(dockerID(env.Namespace, containerdID)); ok && code != 0 {
				exitCode = strconv.Itoa(code)
			}
		}
	case *eventsapi.TaskDelete:
		containerdID = e.ContainerID
	default:
		return dockerEvent{}, false
	}
	if containerdID == "" || env.Namespace == "" {
		return dockerEvent{}, false
	}

	attrs := map[string]string{}
	// Enrich with container labels/name/image like the Docker daemon does.
	// The container may already be gone (delete events) — send what we have.
	nsCtx := namespaces.WithNamespace(ctx, env.Namespace)
	if ctr, err := cl.LoadContainer(nsCtx, containerdID); err == nil {
		if info, err := ctr.Info(nsCtx); err == nil {
			for k, v := range info.Labels {
				attrs[k] = v
			}
			if info.Image != "" {
				attrs["image"] = info.Image
			}
		}
	}
	if name, ok := attrs["nerdctl/name"]; ok {
		attrs["name"] = name
	}
	if exitCode != "" {
		attrs["exitCode"] = exitCode
	}
	ts := env.Timestamp
	return dockerEvent{
		Type:   "container",
		Action: action,
		Scope:  "local",
		Actor: dockerEventActor{
			ID:         dockerID(env.Namespace, containerdID),
			Attributes: attrs,
		},
		Time:     ts.Unix(),
		TimeNano: ts.UnixNano(),
	}, true
}
