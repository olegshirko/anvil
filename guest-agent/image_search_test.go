package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDockerHubSearchTerm(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"nginx", "nginx", false},
		{"bitnami/redis", "bitnami/redis", false},
		{"docker.io/library/nginx", "library/nginx", false},
		{"index.docker.io/nginx", "nginx", false},
		{"ghcr.io/foo", "", true},
		{"localhost:5000/foo", "", true},
	}
	for _, tc := range cases {
		got, err := dockerHubSearchTerm(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("dockerHubSearchTerm(%q) = %q, %v; want %q, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestFilterSearchResults(t *testing.T) {
	in := []dockerSearchResult{
		{Name: "nginx", StarCount: 100, IsOfficial: true},
		{Name: "someone/nginx", StarCount: 5},
		{Name: "other/nginx", StarCount: 1, IsAutomated: true},
	}
	names := func(rs []dockerSearchResult) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return out
	}
	cases := []struct {
		filters string
		want    []string
	}{
		{"", []string{"nginx", "someone/nginx", "other/nginx"}},
		{`{"is-official":["true"]}`, []string{"nginx"}},
		{`{"is-official":{"false":true}}`, []string{"someone/nginx", "other/nginx"}},
		{`{"stars":["3"]}`, []string{"nginx", "someone/nginx"}},
		{`{"is-automated":["true"]}`, []string{"other/nginx"}},
	}
	for _, tc := range cases {
		got, err := filterSearchResults(in, parseDockerFilters(tc.filters))
		if err != nil {
			t.Fatalf("%s: %v", tc.filters, err)
		}
		if g, w := names(got), tc.want; len(g) != len(w) || (len(g) > 0 && g[0] != w[0]) || (len(g) > 1 && g[len(g)-1] != w[len(w)-1]) {
			t.Errorf("filters %s = %v, want %v", tc.filters, g, w)
		}
	}
	if _, err := filterSearchResults(in, parseDockerFilters(`{"stars":["many"]}`)); err == nil {
		t.Error("invalid stars filter accepted")
	}
}

func TestHandleImageSearch(t *testing.T) {
	var gotQuery string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(map[string]any{
			"num_results": 2,
			"results": []map[string]any{
				{"name": "alpine", "star_count": 10000, "is_official": true, "description": "A minimal image"},
				{"name": "someone/alpine", "star_count": 2},
			},
		})
	}))
	defer hub.Close()
	orig := dockerHubSearchURL
	dockerHubSearchURL = hub.URL + "/v1/search"
	t.Cleanup(func() { dockerHubSearchURL = orig })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, `/images/search?term=alpine&limit=5&filters={"is-official":["true"]}`, nil)
	handleImageSearch(rec, req, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if gotQuery != "n=5&q=alpine" {
		t.Errorf("hub query = %q", gotQuery)
	}
	var results []dockerSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Name != "alpine" || !results[0].IsOfficial || results[0].Description != "A minimal image" {
		t.Errorf("results = %+v", results)
	}

	rec = httptest.NewRecorder()
	handleImageSearch(rec, httptest.NewRequest(http.MethodGet, "/images/search", nil), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing term: status %d", rec.Code)
	}
}
