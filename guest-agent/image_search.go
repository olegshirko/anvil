package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// `docker search`: the Docker Hub v1 search API, queried the way dockerd
// does it, with dockerd's filters applied locally. Only Docker Hub is
// searchable; other registries have no standard search endpoint.

var dockerHubSearchURL = "https://index.docker.io/v1/search"

type dockerSearchResult struct {
	StarCount   int    `json:"star_count"`
	IsOfficial  bool   `json:"is_official"`
	Name        string `json:"name"`
	IsAutomated bool   `json:"is_automated"`
	Description string `json:"description"`
}

func handleImageSearch(w http.ResponseWriter, r *http.Request, _ routeParams) {
	q := r.URL.Query()
	term := q.Get("term")
	if term == "" {
		writeJSONError(w, http.StatusBadRequest, "search term is required")
		return
	}
	limit := 25
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeJSONError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	term, err := dockerHubSearchTerm(term)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	results, err := searchDockerHub(r.Context(), term, limit)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, explainEgressFailure(err).Error())
		return
	}
	results, err = filterSearchResults(results, parseDockerFilters(q.Get("filters")))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// dockerHubSearchTerm strips a Docker Hub host prefix and refuses any other
// registry.
func dockerHubSearchTerm(term string) (string, error) {
	host, rest, ok := strings.Cut(term, "/")
	if !ok || !(strings.ContainsAny(host, ".:") || host == "localhost") {
		return term, nil
	}
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return rest, nil
	}
	return "", fmt.Errorf("search is only supported on Docker Hub, not %s", host)
}

func searchDockerHub(ctx context.Context, term string, limit int) ([]dockerSearchResult, error) {
	u := dockerHubSearchURL + "?" + url.Values{"q": {term}, "n": {strconv.Itoa(limit)}}.Encode()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: registryTransport()}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker hub search: %s", resp.Status)
	}
	var body struct {
		Results []dockerSearchResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("docker hub search: %w", err)
	}
	if len(body.Results) > limit {
		body.Results = body.Results[:limit]
	}
	return body.Results, nil
}

// filterSearchResults applies dockerd's search filters: is-official,
// is-automated (true/false) and stars (minimum star count).
func filterSearchResults(in []dockerSearchResult, filters map[string]map[string]bool) ([]dockerSearchResult, error) {
	boolFilter := func(key string) (*bool, error) {
		var val *bool
		for v := range filters[key] {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("invalid filter '%s=%s'", key, v)
			}
			val = &b
		}
		return val, nil
	}
	official, err := boolFilter("is-official")
	if err != nil {
		return nil, err
	}
	automated, err := boolFilter("is-automated")
	if err != nil {
		return nil, err
	}
	minStars := 0
	for v := range filters["stars"] {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid filter 'stars=%s'", v)
		}
		minStars = max(minStars, n)
	}
	out := []dockerSearchResult{}
	for _, r := range in {
		if official != nil && r.IsOfficial != *official {
			continue
		}
		if automated != nil && r.IsAutomated != *automated {
			continue
		}
		if r.StarCount < minStars {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
