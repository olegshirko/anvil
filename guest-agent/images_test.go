package main

import "testing"

// canonicalizeImageRef had a regression where a bare "name:tag" was treated
// as registry-qualified, skipping docker.io/library/ normalization — see
// IMPROVEMENTS.md §2. These cases pin the full matrix.
func TestCanonicalizeImageRef(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"postgres:15.5":                     "docker.io/library/postgres:15.5",
		"nginx":                             "docker.io/library/nginx:latest",
		"myimg:1":                           "docker.io/library/myimg:1",
		"foo/bar":                           "docker.io/foo/bar:latest",
		"foo/bar:1.2":                       "docker.io/foo/bar:1.2",
		"ghcr.io/foo/bar":                   "ghcr.io/foo/bar:latest",
		"localhost:5000/foo":                "localhost:5000/foo:latest",
		"registry.example.com:8443/foo/bar": "registry.example.com:8443/foo/bar:latest",
		// Digest refs keep the digest, no :latest appended.
		"postgres@sha256:deadbeef": "docker.io/library/postgres@sha256:deadbeef",
		"ghcr.io/foo@sha256:beef":  "ghcr.io/foo@sha256:beef",
	}
	for in, want := range cases {
		if got := canonicalizeImageRef(in); got != want {
			t.Errorf("canonicalizeImageRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitImageTag(t *testing.T) {
	type tc struct {
		ref, name, tag string
		ok             bool
	}
	cases := []tc{
		{"nginx", "nginx", "latest", true},
		{"postgres:15.5", "postgres", "15.5", true},
		{"foo/bar:1", "foo/bar", "1", true},
		{"ghcr.io/foo/bar:1", "ghcr.io/foo/bar", "1", true},
		// A ":" before the last "/" is a registry port, not a tag.
		{"host:5000/foo", "host:5000/foo", "latest", true},
		// Digest refs are not mirrorable.
		{"repo@sha256:abc", "", "", false},
	}
	for _, c := range cases {
		name, tag, ok := splitImageTag(c.ref)
		if name != c.name || tag != c.tag || ok != c.ok {
			t.Errorf("splitImageTag(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.ref, name, tag, ok, c.name, c.tag, c.ok)
		}
	}
}

func TestSplitRepoTag(t *testing.T) {
	cases := map[string][2]string{
		"docker.io/library/nginx:latest":    {"docker.io/library/nginx", "latest"},
		"ghcr.io/foo/bar":                   {"ghcr.io/foo/bar", ""},
		"docker.io/library/postgres@sha256:abc": {"docker.io/library/postgres", ""},
	}
	for in, want := range cases {
		repo, tag := splitRepoTag(in)
		if repo != want[0] || tag != want[1] {
			t.Errorf("splitRepoTag(%q) = (%q, %q), want (%q, %q)", in, repo, tag, want[0], want[1])
		}
	}
}

// dedupeStrings preserves order while dropping duplicates.
func TestDedupeStrings(t *testing.T) {
	got := dedupeStrings([]string{"a", "b", "a", "c", "b"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("dedupe = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupe = %v, want %v", got, want)
		}
	}
}
