package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"

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
// Historical replay (?since) and filters are not supported; only task events
// for containers are forwarded.
func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"message":"streaming not supported"}`, http.StatusInternalServerError)
		return
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
			debugLog("events: send %s id=%s attrs=%v", ev.Action, ev.Actor.ID[:12], ev.Actor.Attributes)
			if err := enc.Encode(ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
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
