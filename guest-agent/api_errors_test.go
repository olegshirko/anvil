package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/containerd/errdefs"
)

func TestErrorStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("No such container: web"), http.StatusNotFound},
		{fmt.Errorf("No such image: x"), http.StatusNotFound},
		{fmt.Errorf("load: %w", errdefs.ErrNotFound), http.StatusNotFound},
		{fmt.Errorf("create: %w", errdefs.ErrAlreadyExists), http.StatusConflict},
		{errConflict("name in use"), http.StatusConflict},
		{fmt.Errorf("wrapped: %w", errNotModified("running")), http.StatusNotModified},
		{errors.New("boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := errorStatus(c.err, http.StatusInternalServerError); got != c.want {
			t.Errorf("errorStatus(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestWriteAPIErrorNotModifiedHasNoBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAPIError(rec, errNotModified("already running"), http.StatusInternalServerError)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("got %d with body %q", rec.Code, rec.Body.String())
	}
}

func TestExecStorePrunes(t *testing.T) {
	s := newExecStore()
	old := &execSpec{ID: "old", Namespace: "ns", ContainerdID: "c1"}
	old.setExit(0)
	old.finished = old.finished.Add(-2 * execRetention)
	live := &execSpec{ID: "live", Namespace: "ns", ContainerdID: "c1", running: true}
	s.add(old)
	s.add(live)
	s.add(&execSpec{ID: "other", Namespace: "ns", ContainerdID: "c2"})
	if s.get("old") != nil {
		t.Error("finished exec past retention was kept")
	}
	if s.get("live") == nil {
		t.Error("running exec was pruned")
	}
	s.forgetContainer("ns", "c1")
	if s.get("live") != nil || s.get("other") == nil {
		t.Error("forgetContainer dropped the wrong execs")
	}
}
