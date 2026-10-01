package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// pruneFilter is what the prune endpoints honor: until=<time> (objects
// created before it) and label / label! constraints. Ignoring them made
// `docker container prune --filter label=…` — and Testcontainers' Ryuk,
// which prunes by its session label — remove everything unused.
type pruneFilter struct {
	until   time.Time
	filters map[string]map[string]bool
}

// newPruneFilter validates the filter keys (extra lists endpoint-specific
// ones, e.g. dangling) and parses until.
func newPruneFilter(filters map[string]map[string]bool, extra ...string) (pruneFilter, error) {
	allowed := map[string]bool{"until": true, "label": true, "label!": true}
	for _, k := range extra {
		allowed[k] = true
	}
	if err := validateFilterKeys(filters, allowed); err != nil {
		return pruneFilter{}, err
	}
	pf := pruneFilter{filters: filters}
	for v := range filters["until"] {
		t, err := parseUntil(v, time.Now())
		if err != nil {
			return pruneFilter{}, &apiError{status: http.StatusBadRequest, msg: err.Error()}
		}
		if pf.until.IsZero() || t.Before(pf.until) {
			pf.until = t
		}
	}
	return pf, nil
}

// parseUntil accepts what Docker does: a Go duration relative to now
// ("24h"), unix seconds (optionally fractional) or an RFC 3339 time/date.
func parseUntil(v string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(-d), nil
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9)), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid until filter %q", v)
}

// keep reports whether an object with these labels and creation time
// passes the filter (a zero created time never passes until).
func (pf pruneFilter) keep(labels map[string]string, created time.Time) bool {
	if !pf.until.IsZero() && (created.IsZero() || !created.Before(pf.until)) {
		return false
	}
	return matchesLabelFilters(labels, map[string]map[string]bool{
		"label": pf.filters["label"], "label!": pf.filters["label!"],
	})
}

// fileModTime is a network's creation time: its conflist's mtime.
func fileModTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// parseCreatedAt reads a volume's CreatedAt (RFC 3339).
func parseCreatedAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}
