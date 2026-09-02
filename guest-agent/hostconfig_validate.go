package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// unsupportedHostConfig lists HostConfig fields anvil cannot honor, mapped to
// the CLI flag a user would recognize. Anything here is refused loudly: a
// silently dropped field is a worse bug than a rejected request, because the
// container starts and misbehaves later. Entries are deleted as fields land.
var unsupportedHostConfig = map[string]string{
	"OomKillDisable": "--oom-kill-disable (cgroup v2 has no oom_kill_disable)",
	"BlkioWeight":    "--blkio-weight (needs the bfq io controller)",
	"StorageOpt":     "--storage-opt (needs project quotas on the overlay fs)",
	"Isolation":      "--isolation (Windows containers only)",
	"Runtime":        "--runtime (only runc exists in the guest)",
}

// validateHostConfig re-decodes the raw create body to spot fields the typed
// struct silently drops. It runs before any containerd work.
func validateHostConfig(body []byte) error {
	var envelope struct {
		HostConfig map[string]json.RawMessage `json:"HostConfig"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil // malformed bodies are the decoder's problem, not ours
	}
	for field, flag := range unsupportedHostConfig {
		raw, ok := envelope.HostConfig[field]
		if !ok || isEmptyJSON(raw) {
			continue // the CLI always sends the key; only a set value counts
		}
		return fmt.Errorf("%s is not supported by anvil: %s", field, flag)
	}
	return validateLogConfig(envelope.HostConfig["LogConfig"])
}

// validateLogConfig enforces the LogConfig contract: json-file (what the
// guest already writes) and none (discard) are honored; every other driver
// is refused instead of silently falling back to json-file.
func validateLogConfig(raw json.RawMessage) error {
	if len(raw) == 0 || isEmptyJSON(raw) {
		return nil
	}
	var lc struct {
		Type string `json:"Type"`
	}
	if err := json.Unmarshal(raw, &lc); err != nil {
		return nil // shape errors surface in the typed decode
	}
	switch lc.Type {
	case "", "json-file", "none":
		return nil
	}
	return fmt.Errorf("log driver %q is not supported by anvil: only json-file and none exist", lc.Type)
}

// isEmptyJSON reports whether a raw JSON value carries no meaningful
// setting. The Docker CLI populates the whole HostConfig object on every
// request, so presence alone means nothing: null/0/""/[]/{}/false all count
// as unset.
func isEmptyJSON(raw json.RawMessage) bool {
	switch string(trimJSONSpace(raw)) {
	case "", "null", "0", `""`, "[]", "{}", "false":
		return true
	}
	return false
}

func trimJSONSpace(raw json.RawMessage) []byte {
	start, end := 0, len(raw)
	for start < end && isJSONSpace(raw[start]) {
		start++
	}
	for end > start && isJSONSpace(raw[end-1]) {
		end--
	}
	return raw[start:end]
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// validateCreatePlatform refuses platform requests the guest cannot honor.
// Until Rosetta lands, anything but linux/arm64 is a lie — a user asking for
// amd64 deserves an answer, not an arm64 container pretending to be one.
func validateCreatePlatform(r *http.Request) error {
	p := r.URL.Query().Get("platform")
	if p == "" || p == "linux" || p == "linux/arm64" {
		return nil
	}
	return fmt.Errorf("platform %q is not supported by anvil: the guest VM is arm64-only (no Rosetta emulation yet)", p)
}

// memorySwapSpec converts Docker's MemorySwap into the OCI
// LinuxMemory.Swap value. Verified empirically against the guest's runc:
// the cgroup-v2 shim applies the v1→v2 conversion itself (writes
// swap.max = Swap − Memory), so the spec must carry docker's combined
// value unchanged — converting here would subtract Memory twice.
// Returns nil for "unset".
func memorySwapSpec(memory, memorySwap int64) (*int64, error) {
	switch {
	case memorySwap == 0:
		return nil, nil
	case memorySwap < 0:
		unlimited := int64(-1)
		return &unlimited, nil // docker's -1: unlimited swap
	case memory <= 0:
		return nil, fmt.Errorf("--memory-swap requires --memory")
	case memorySwap < memory:
		return nil, fmt.Errorf("--memory-swap (%d) must be >= --memory (%d)", memorySwap, memory)
	default:
		return &memorySwap, nil
	}
}

// knownRlimits is the set of rlimit types runc accepts (RLIMIT_-prefixed).
// Docker sends bare names ("nofile"); unknown ones are rejected here because
// runc's own error is far less legible.
var knownRlimits = map[string]bool{
	"RLIMIT_AS":         true,
	"RLIMIT_CORE":       true,
	"RLIMIT_CPU":        true,
	"RLIMIT_DATA":       true,
	"RLIMIT_FSIZE":      true,
	"RLIMIT_LOCKS":      true,
	"RLIMIT_MEMLOCK":    true,
	"RLIMIT_MSGQUEUE":   true,
	"RLIMIT_NICE":       true,
	"RLIMIT_NOFILE":     true,
	"RLIMIT_NPROC":      true,
	"RLIMIT_RSS":        true,
	"RLIMIT_RTPRIO":     true,
	"RLIMIT_RTTIME":     true,
	"RLIMIT_SIGPENDING": true,
	"RLIMIT_STACK":      true,
}
