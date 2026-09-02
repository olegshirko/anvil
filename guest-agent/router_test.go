package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouteMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          routeParams // nil = no match
	}{
		{"/containers/json", "/containers/json", routeParams{}},
		{"/containers/json", "/containers/xyz", nil},
		{"/containers/:id/start", "/containers/abc123/start", routeParams{"id": "abc123"}},
		{"/containers/:id/start", "/containers/abc123/stop", nil},
		{"/containers/:id/start", "/containers/start", nil},
		{"/containers/:id", "/containers/abc123", routeParams{"id": "abc123"}},
		{"/containers/:id/archive", "/containers/abc/archive", routeParams{"id": "abc"}},
		// Wildcard captures slashes (image references).
		{"/images/*name/json", "/images/docker.io/library/alpine/json",
			routeParams{"name": "docker.io/library/alpine"}},
		{"/images/*name", "/images/alpine:3.20", routeParams{"name": "alpine:3.20"}},
		{"/images/*name/json", "/images/json", nil}, // wildcard segment must be non-empty and json is literal elsewhere
	}
	for _, tc := range cases {
		rt := newRoute("", tc.pattern, func(http.ResponseWriter, *http.Request, routeParams) {})
		got, ok := rt.match(splitPath(tc.path))
		if ok != (tc.want != nil) {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.path, ok, tc.want != nil)
			continue
		}
		if tc.want == nil {
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("match(%q, %q) params = %v, want %v", tc.pattern, tc.path, got, tc.want)
			continue
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("match(%q, %q) params = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		}
	}
}

func TestDispatchRouteSelection(t *testing.T) {
	var hit string
	reset := func(routes []apiRoute) {
		orig := dockerRoutes
		dockerRoutes = routes
		t.Cleanup(func() { dockerRoutes = orig })
	}

	// Literal routes win over parameterized ones with the same shape.
	reset([]apiRoute{
		newRoute(http.MethodGet, "/containers/json", func(w http.ResponseWriter, _ *http.Request, _ routeParams) {
			hit = "list"
		}),
		newRoute(http.MethodDelete, "/containers/:id", func(w http.ResponseWriter, _ *http.Request, p routeParams) {
			hit = "delete:" + p["id"]
		}),
	})

	rec := httptest.NewRecorder()
	dispatchDockerAPI(rec, httptest.NewRequest(http.MethodGet, "/containers/json", nil), "/containers/json")
	if hit != "list" {
		t.Errorf("GET /containers/json hit %q, want list", hit)
	}

	rec = httptest.NewRecorder()
	dispatchDockerAPI(rec, httptest.NewRequest(http.MethodDelete, "/containers/xyz", nil), "/containers/xyz")
	if hit != "delete:xyz" {
		t.Errorf("DELETE /containers/xyz hit %q, want delete:xyz", hit)
	}

	// Wrong method on a routed path is a 404 (no 405 route shadowing).
	hit = ""
	rec = httptest.NewRecorder()
	dispatchDockerAPI(rec, httptest.NewRequest(http.MethodPost, "/containers/json", nil), "/containers/json")
	if hit != "" || rec.Code != http.StatusNotFound {
		t.Errorf("POST /containers/json: hit=%q code=%d, want 404", hit, rec.Code)
	}

	// Unknown path is a 404.
	rec = httptest.NewRecorder()
	dispatchDockerAPI(rec, httptest.NewRequest(http.MethodGet, "/nope", nil), "/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope code=%d, want 404", rec.Code)
	}
}

// TestDockerRoutesSmoke walks every registered route through the matcher so a
// malformed table entry fails here instead of at runtime in the VM.
func TestDockerRoutesSmoke(t *testing.T) {
	if len(dockerRoutes) == 0 {
		t.Fatal("dockerRoutes is empty")
	}
	for _, rt := range dockerRoutes {
		if rt.handler == nil || len(rt.pattern) == 0 {
			t.Errorf("route %v: nil handler or empty pattern", rt.pattern)
		}
		for _, seg := range rt.pattern {
			if seg == "" {
				t.Errorf("route %v: empty segment", rt.pattern)
			}
		}
	}
}
