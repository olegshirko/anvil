package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestSplitMirrorForcedRef(t *testing.T) {
	cases := []struct {
		ref      string
		wantRef  string
		wantOK   bool
	}{
		{"mirror.anvil/postgres:15.5", "postgres:15.5", true},
		{"mirror.anvil/library/nginx", "library/nginx", true},
		{"mirror.anvil/apache/kafka:4.1.1", "apache/kafka:4.1.1", true},
		{"postgres:15.5", "", false},
		{"docker.io/library/postgres:15.5", "", false},
		{"myreg.io/postgres:15.5", "", false},
		{"mirror.anvil", "", false},           // no image at all
		{"mirror.anvil/", "", false},          // empty image
	}
	for _, tc := range cases {
		got, ok := splitMirrorForcedRef(tc.ref)
		if ok != tc.wantOK || got != tc.wantRef {
			t.Errorf("splitMirrorForcedRef(%q) = %q, %v; want %q, %v", tc.ref, got, ok, tc.wantRef, tc.wantOK)
		}
	}
}

func TestMirrorReleaseAsset(t *testing.T) {
	cases := []struct {
		ref      string
		platform string
		release  string
		asset    string
		wantErr  string
	}{
		{"postgres:15.5", "", "postgres-15.5-arm64", "image-arm64.tar.zst", ""},
		{"postgres:15.5", "linux/arm64", "postgres-15.5-arm64", "image-arm64.tar.zst", ""},
		{"apache/kafka:4.1.1", "linux/amd64", "apache-kafka-4.1.1-amd64", "image-amd64.tar.zst", ""},
		{"alpine:3.20", "linux/riscv64", "", "", "not mirrored"},
		{"postgres@sha256:abc", "", "", "", "not found in docker-mirror"},
	}
	for _, tc := range cases {
		release, asset, err := mirrorReleaseAsset(tc.ref, tc.platform)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("mirrorReleaseAsset(%q, %q) err = %v, want it to name %q", tc.ref, tc.platform, err, tc.wantErr)
			}
			continue
		}
		if err != nil || release != tc.release || asset != tc.asset {
			t.Errorf("mirrorReleaseAsset(%q, %q) = %q, %q, %v; want %q, %q", tc.ref, tc.platform, release, asset, err, tc.release, tc.asset)
		}
	}
}

// stubMirrorTransport answers every request to github.com with the given
// status and fails loudly on any other host: a forced mirror pull must not
// touch Docker Hub or any other registry.
type stubMirrorTransport struct {
	status int
	seen   []string
}

func (s *stubMirrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.seen = append(s.seen, req.URL.Host+req.URL.Path)
	if req.URL.Host != "github.com" {
		t := &testing.T{}
		t.Errorf("forced mirror pull contacted %s", req.URL.Host)
		return nil, context.Canceled
	}
	return &http.Response{StatusCode: s.status, Body: http.NoBody, Header: http.Header{}}, nil
}

func TestForcedMirrorPullNotFound(t *testing.T) {
	stub := &stubMirrorTransport{status: http.StatusNotFound}
	old := mirrorHTTPClient
	mirrorHTTPClient = &http.Client{Transport: stub}
	t.Cleanup(func() { mirrorHTTPClient = old })

	err := loadFromMirrorPlatform(context.Background(), "postgres:9.9", "default", "")
	if err != errMirrorNotFound {
		t.Fatalf("err = %v, want errMirrorNotFound", err)
	}
	if len(stub.seen) == 0 || !strings.HasPrefix(stub.seen[0], "github.com/") {
		t.Fatalf("requests = %v, want a github.com release-asset URL", stub.seen)
	}
	if strings.Contains(stub.seen[0], "9.9") && !strings.Contains(stub.seen[0], "postgres-9.9-arm64") {
		t.Fatalf("url %q does not follow the release naming", stub.seen[0])
	}
}
