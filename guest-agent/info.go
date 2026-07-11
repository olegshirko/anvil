package main

import (
	"encoding/json"
	"net/http"
	"runtime"
)

// handleDockerInfo returns a minimal Docker Engine info response.
func handleDockerInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]interface{}{
		"ID":              "anvil-vz-runner",
		"Containers":      0,
		"Images":          0,
		"Driver":          "overlayfs",
		"SecurityOptions": []string{},
		"Architecture":    runtime.GOARCH,
		"OSType":          "linux",
		"NCPU":            runtime.NumCPU(),
		"MemTotal":        int64(0),
		"ServerVersion":   "24.0.0",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
	_ = r
}
