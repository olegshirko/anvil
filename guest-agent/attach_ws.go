package main

import (
	"io"
	"net/http"

	"golang.org/x/net/websocket"
)

// handleAttachWS implements GET /containers/{id}/attach/ws: attach over a
// WebSocket, as web consoles use it. As in dockerd, the frames carry the
// raw output (never multiplexed) and the client's input goes to stdin.
func handleAttachWS(w http.ResponseWriter, r *http.Request, id string) {
	ns, containerdID, _, err := resolveDockerID(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	q := r.URL.Query()
	ao := attachStreamOptions{
		logs:   queryBool(q, "logs"),
		stream: queryBool(q, "stream"),
		stdout: queryBool(q, "stdout"),
		stderr: queryBool(q, "stderr"),
	}
	if !q.Has("stdout") && !q.Has("stderr") {
		ao.stdout, ao.stderr = true, true
	}
	if !q.Has("stream") && !q.Has("logs") {
		ao.logs, ao.stream = true, true
	}
	did := dockerID(ns, containerdID)
	meta, _ := loadContainerMeta(ns, containerdID)
	var stdin *containerStdin
	if queryBool(q, "stdin") && meta != nil && meta.OpenStdin {
		running, _, _ := containerTaskState(r.Context(), ns, containerdID)
		if stdin, err = openContainerStdin(ns, containerdID, !running); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	server := websocket.Server{
		// dockerd accepts any origin: the Docker socket is the authority.
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			defer ws.Close()
			ws.PayloadType = websocket.BinaryFrame
			attachBegin(did)
			defer attachEnd(did)
			go func() {
				if stdin == nil {
					io.Copy(io.Discard, ws) //nolint:errcheck
					return
				}
				io.Copy(stdin, ws) //nolint:errcheck
				if meta.StdinOnce {
					stdin.close()
				}
			}()
			// tty=true: raw bytes, which is what attach/ws always sends.
			streamTaskLogToTTY(ws, ns, containerdID, true, ao)
		},
	}
	server.ServeHTTP(w, r)
}
