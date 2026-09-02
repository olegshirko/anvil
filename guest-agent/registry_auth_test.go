package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func authHeader(t *testing.T, payload map[string]string) string {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func TestParseRegistryAuth(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/images/create", nil)
	if a := parseRegistryAuth(r); a != nil {
		t.Fatalf("missing header should be nil, got %+v", a)
	}
	r.Header.Set("X-Registry-Auth", "!!!not-base64!!!")
	if a := parseRegistryAuth(r); a != nil {
		t.Fatalf("undecodable header should be nil, got %+v", a)
	}
	r.Header.Set("X-Registry-Auth", authHeader(t, map[string]string{
		"username":      "user",
		"password":      "pass",
		"serveraddress": "https://ghcr.io/v1/",
	}))
	a := parseRegistryAuth(r)
	if a == nil || a.Username != "user" || a.Password != "pass" {
		t.Fatalf("bad decode: %+v", a)
	}
	if !a.matchesHost("ghcr.io") || a.matchesHost("docker.io") {
		t.Fatalf("host matching failed for %+v", a)
	}
}

func TestRegistryAPIBasesSchemeOrder(t *testing.T) {
	// Remote registries — including non-standard ports — are HTTPS first,
	// plain HTTP only as a fallback.
	bases := registryAPIBases("registry.corp:5000")
	if len(bases) != 2 || !strings.HasPrefix(bases[0], "https://") || !strings.HasPrefix(bases[1], "http://") {
		t.Fatalf("remote host: got %v", bases)
	}
	// Localhost is the only host that may default to plain HTTP.
	for _, host := range []string{"localhost", "localhost:5000", "127.0.0.1:5000", "[::1]:5000", "::1"} {
		bases := registryAPIBases(host)
		if len(bases) != 1 || !strings.HasPrefix(bases[0], "http://") {
			t.Fatalf("localhost-form %q: got %v", host, bases)
		}
	}
}

func TestBearerTokenURLScopeless(t *testing.T) {
	u, err := bearerTokenURL(`Bearer realm="https://auth.example.com/token",service="registry.example.com"`)
	if err != nil {
		t.Fatal(err)
	}
	// docker login requests no scope; some registries 403 unknown scopes.
	if strings.Contains(u, "scope=") {
		t.Fatalf("token URL must not request a scope: %s", u)
	}
	if !strings.Contains(u, "service=registry.example.com") {
		t.Fatalf("missing service param: %s", u)
	}
	if _, err := bearerTokenURL(`Bearer service="x"`); err == nil {
		t.Fatal("challenge without realm must error")
	}
}

func TestRegistryAuthHubAliases(t *testing.T) {
	for _, host := range []string{"index.docker.io", "registry-1.docker.io", "docker.io", "https://index.docker.io/v1/"} {
		if got := registryHostOf(host); got != "docker.io" {
			t.Fatalf("registryHostOf(%q) = %q, want docker.io", host, got)
		}
	}
	if got := registryHostOf(" Harbor.example.com:8443/v1/ "); got != "harbor.example.com:8443" {
		t.Fatalf("registryHostOf with port/path = %q", got)
	}
	a := &registryAuth{Username: "u", Password: "p", ServerAddress: "https://index.docker.io/v1/"}
	if !a.matchesHost("registry-1.docker.io") {
		t.Fatal("hub creds must match registry-1 endpoint")
	}
}

func TestRegistryAuthEmptyServerMatchesAnyHost(t *testing.T) {
	a := &registryAuth{Username: "u", Password: "p"}
	if !a.matchesHost("ghcr.io") || !a.matchesHost("registry-1.docker.io") {
		t.Fatal("creds without server address should apply everywhere")
	}
	if a.empty() {
		t.Fatal("creds with user/password are not empty")
	}
	if (&registryAuth{}).empty() != true || (*registryAuth)(nil).empty() != true {
		t.Fatal("nil and zero-value auth must be empty")
	}
}

func TestAuthResponseErrorsNeverFallBackToPlainHTTP(t *testing.T) {
	// A registry that ANSWERED (even with 401/500) must fail the login
	// outright: falling through to the http:// candidate would send the same
	// Authorization header in cleartext. Errors derived from a response must
	// not be classified as errRegistryUnreachable (the fall-through marker).
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized) // wrong creds
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer reg.Close()

	err := validateRegistryLogin(context.Background(), &registryAuth{
		Username: "u", Password: "p", ServerAddress: reg.URL,
	})
	if err == nil {
		t.Fatal("wrong creds must fail")
	}
	if errors.Is(err, errRegistryUnreachable) {
		t.Fatalf("response-derived error must not allow plain-HTTP fallback: %v", err)
	}
}

func TestAuthEndpoint(t *testing.T) {
	// Token endpoint: accepts only the good basic creds.
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); ok && user == "good" && pass == "pw" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer tsrv.Close()

	// Registry issuing a bearer challenge pointing at the token endpoint.
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+tsrv.URL+`/token",service="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer reg.Close()

	doLogin := func(user string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"username": user, "password": "pw", "serveraddress": reg.URL})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/auth", bytes.NewReader(body))
		handleAuth(w, req)
		return w
	}
	if w := doLogin("good"); w.Code != http.StatusOK {
		t.Fatalf("valid creds: got %d %s", w.Code, w.Body.String())
	}
	if w := doLogin("bad"); w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid creds: got %d %s", w.Code, w.Body.String())
	}
}
