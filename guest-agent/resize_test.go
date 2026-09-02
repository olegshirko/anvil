package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseResizeQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/containers/abc/resize?h=40&w=120", nil)
	w, h, err := parseResizeQuery(r)
	if err != nil || w != 120 || h != 40 {
		t.Fatalf("got %d x %d, err=%v", w, h, err)
	}
	for _, q := range []string{"", "?h=0&w=120", "?h=40", "?h=abc&w=120"} {
		r := httptest.NewRequest(http.MethodPost, "/containers/abc/resize"+q, nil)
		if _, _, err := parseResizeQuery(r); err == nil {
			t.Fatalf("query %q should be invalid", q)
		}
	}
}
