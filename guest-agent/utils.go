package main

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
)

const (
	listenPort        = 1024
	dockerAPIPort     = 1025
	hostPortCheckPort = 1027
	containerdSocket  = "/run/containerd/containerd.sock"
	cniConfDir        = "/etc/cni/net.d"
)

var debugMode = os.Getenv("ANVIL_DEBUG") == "1" || os.Getenv("ANVIL_DEBUG") == "true"

// debugLog prints a log line only when ANVIL_DEBUG is enabled.
func debugLog(format string, v ...interface{}) {
	if debugMode {
		log.Printf("[debug] "+format, v...)
	}
}

// defaultString returns fallback if s is empty.
func defaultString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	re := regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")
	return re.ReplaceAllString(s, "")
}

// dockerID returns a deterministic 64-hex Docker-compatible ID for a
// containerd container. It is stable across restarts because it is derived
// from the namespace and containerd ID.
func dockerID(namespace, containerID string) string {
	h := sha256.Sum256([]byte(namespace + "/" + containerID))
	return fmt.Sprintf("%x", h)[:64]
}

// dockerState maps a containerd task status to a Docker container State.
func dockerState(status string) string {
	switch status {
	case "running":
		return "running"
	case "stopped":
		return "exited"
	case "paused":
		return "paused"
	default:
		return "created"
	}
}

// dockerStatus maps a containerd task status to a Docker container Status.
func dockerStatus(status string) string {
	switch status {
	case "running":
		return "running"
	case "stopped":
		return "exited"
	case "paused":
		return "paused"
	default:
		return "created"
	}
}

// namespaceFromNetwork derives the containerd namespace from Docker's NetworkMode.
func namespaceFromNetwork(networkMode string) string {
	if networkMode == "" || networkMode == "default" || networkMode == "bridge" {
		return "default"
	}
	if strings.HasSuffix(networkMode, "_default") {
		return strings.TrimSuffix(networkMode, "_default")
	}
	return networkMode
}
