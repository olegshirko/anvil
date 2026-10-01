package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/containerd/errdefs"
)

// apiError carries the HTTP status Docker uses for a failure. Clients branch
// on it: the CLI treats a 404 from `rm -f` as success, compose tolerates a
// container removed under it only on 404, and 304 from start/stop means
// "already in that state", not an error.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errConflict(format string, args ...any) error {
	return &apiError{status: http.StatusConflict, msg: fmt.Sprintf(format, args...)}
}

// errInvalid is a 400: the request asks for something anvil cannot do.
func errInvalid(format string, args ...any) error {
	return &apiError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

func errNotModified(format string, args ...any) error {
	return &apiError{status: http.StatusNotModified, msg: fmt.Sprintf(format, args...)}
}

// errorStatus maps err to its Docker API status, or fallback.
func errorStatus(err error, fallback int) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status
	}
	switch {
	case errdefs.IsNotFound(err):
		return http.StatusNotFound
	case errdefs.IsConflict(err), errdefs.IsAlreadyExists(err):
		return http.StatusConflict
	}
	// Lookups across the agent report misses as Docker does: "No such
	// container: x", "No such image: y", ...
	if strings.HasPrefix(err.Error(), "No such ") {
		return http.StatusNotFound
	}
	return fallback
}

// writeAPIError writes err with its Docker status (fallback when it has none).
func writeAPIError(w http.ResponseWriter, err error, fallback int) {
	status := errorStatus(err, fallback)
	if status == http.StatusNotModified {
		w.WriteHeader(status) // a 304 has no body
		return
	}
	writeJSONError(w, status, err.Error())
}
