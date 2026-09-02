package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes/docker"
)

// registryAuth mirrors the AuthConfig JSON the Docker CLI base64-encodes into
// the X-Registry-Auth header on pull/push/create/build requests.
type registryAuth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress"`
	IdentityToken string `json:"identitytoken"`
}

// empty reports whether the decoded header carries no usable credentials.
func (a *registryAuth) empty() bool {
	return a == nil || (a.Username == "" && a.Password == "" && a.IdentityToken == "")
}

// parseRegistryAuth decodes the X-Registry-Auth header (base64 AuthConfig
// JSON). Missing or undecodable headers return nil — an anonymous request,
// not an error, so behavior without the header is unchanged.
func parseRegistryAuth(r *http.Request) *registryAuth {
	raw := r.Header.Get("X-Registry-Auth")
	if raw == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(raw)
		if err != nil {
			log.Printf("[registry-auth] undecodable X-Registry-Auth header: %v", err)
			return nil
		}
	}
	var a registryAuth
	if err := json.Unmarshal(decoded, &a); err != nil {
		log.Printf("[registry-auth] malformed X-Registry-Auth payload: %v", err)
		return nil
	}
	return &a
}

// registryHostOf normalizes a registry reference host for comparison:
// scheme and path are dropped and Docker Hub aliases collapse to "docker.io".
func registryHostOf(server string) string {
	s := strings.TrimSpace(server)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i != -1 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/"); i != -1 {
		s = s[:i]
	}
	if i := strings.Index(s, "@"); i != -1 { // user@host
		s = s[i+1:]
	}
	s = strings.ToLower(s)
	switch s {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return s
}

// matchesHost reports whether these credentials are intended for the given
// registry host. Credentials without a server address (the CLI omits it for
// Docker Hub logins) apply to any host — the authorizer asks per host and
// simply gets no useful creds elsewhere.
func (a *registryAuth) matchesHost(host string) bool {
	if a == nil {
		return false
	}
	if a.ServerAddress == "" {
		return true
	}
	return registryHostOf(a.ServerAddress) == registryHostOf(host)
}

// credentials adapts the decoded header to containerd's credential function.
// An identity token is returned as the secret with an empty username, which
// the Docker authorizer treats as a bearer/identity token.
func (a *registryAuth) credentials(host string) (string, string, error) {
	if !a.matchesHost(host) {
		return "", "", nil
	}
	if a.IdentityToken != "" {
		return "", a.IdentityToken, nil
	}
	return a.Username, a.Password, nil
}

// authHTTPClient bounds registry interactions so a hung or unreachable
// registry cannot wedge docker login / docker pull forever. Response-header
// (not total) timeout: blob downloads may legitimately stream for minutes,
// but a server that never answers headers is dead.
var authHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

// authResolverOpts returns pull/push remote options wiring the credentials
// through containerd's Docker registry authorizer. Anonymous requests get no
// options, preserving the default resolver behavior.
func authResolverOpts(a *registryAuth) []client.RemoteOpt {
	if a.empty() {
		return nil
	}
	creds := a.credentials
	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: docker.ConfigureDefaultRegistries(
			docker.WithClient(authHTTPClient),
			docker.WithAuthorizer(docker.NewDockerAuthorizer(
				docker.WithAuthClient(authHTTPClient),
				docker.WithAuthCreds(creds),
			)),
		),
	})
	return []client.RemoteOpt{client.WithResolver(resolver)}
}

// handleAuth implements POST /auth (docker login). Credentials are validated
// against the registry's /v2/ endpoint: a 200 or 401-with-successful-token is
// a login, a failing auth challenge is rejected with 401.
func handleAuth(w http.ResponseWriter, r *http.Request) {
	var a registryAuth
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed auth payload")
		return
	}
	if a.Username == "" && a.IdentityToken == "" {
		writeJSONError(w, http.StatusBadRequest, "no credentials provided")
		return
	}
	// Identity tokens are issued by external providers (ECR helpers, OIDC);
	// their verification flow is provider-specific, so accept them as-is.
	if a.IdentityToken != "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"Status": "Login Succeeded", "IdentityToken": a.IdentityToken})
		return
	}
	if err := validateRegistryLogin(r.Context(), &a); err != nil {
		log.Printf("[registry-auth] login to %s failed: %v", a.ServerAddress, err)
		writeJSONError(w, http.StatusUnauthorized, fmt.Sprintf("login failed: %s", err.Error()))
		return
	}
	log.Printf("[registry-auth] login succeeded for %s", a.ServerAddress)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"Status": "Login Succeeded", "IdentityToken": ""})
}

// registryAPIBases returns candidate /v2/ URLs for a registry host, best
// scheme first. Plain HTTP is insecure and only acceptable for local
// development registries; remote registries on non-standard ports
// (registry.corp:5000) are HTTPS in practice — Docker itself treats only
// localhost as insecure by default. For remote hosts HTTP is still tried as
// a fallback when HTTPS does not answer (misconfigured private regs).
func registryAPIBases(host string) []string {
	// Compare the host part without the port (net.SplitHostPort errors on a
	// bare IPv6 literal like "::1", leaving hostname as-is — which is correct).
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	if hostname == "localhost" || strings.HasPrefix(hostname, "127.") || hostname == "[::1]" || hostname == "::1" {
		return []string{"http://" + host + "/v2/"}
	}
	return []string{"https://" + host + "/v2/", "http://" + host + "/v2/"}
}

// errRegistryUnreachable marks transport-level failures (dial, TLS, timeout):
// only these may fall through to the next candidate scheme. Anything else —
// the registry ANSWERED — must be reported as-is, never retried over plain
// HTTP: a man-in-the-middle answering 401/500 on the HTTPS port would
// otherwise harvest the Authorization header sent to the http:// fallback.
var errRegistryUnreachable = errors.New("registry unreachable")

// validateRegistryLogin checks username/password against the registry auth
// challenge (Basic challenge answered directly, Bearer challenge answered by
// fetching a token with the credentials).
func validateRegistryLogin(ctx context.Context, a *registryAuth) error {
	host := registryHostOf(a.ServerAddress)
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}

	var lastErr error
	for _, apiBase := range registryAPIBases(host) {
		err := tryRegistryAuth(ctx, apiBase, a)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errRegistryUnreachable) {
			return err // the registry responded — its verdict is final
		}
		lastErr = err
	}
	return lastErr
}

// tryRegistryAuth validates credentials against one /v2/ base URL. Transport
// errors mean the scheme/host is wrong — the caller falls through to the next
// candidate base.
func tryRegistryAuth(ctx context.Context, apiBase string, a *registryAuth) error {
	resp, err := authGet(ctx, apiBase, "")
	if err != nil {
		return fmt.Errorf("%w: %v", errRegistryUnreachable, err)
	}
	resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		// 200: open registry (no challenge) — the creds are trivially valid.
		// 404: not a registry /v2/ endpoint; can't verify, accept rather than
		// break logins against registries with unusual routing.
		return nil
	case http.StatusUnauthorized:
	default:
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	challenge := strings.ToLower(resp.Header.Get("WWW-Authenticate"))
	switch {
	case strings.HasPrefix(challenge, "basic"):
		resp2, err := authGet(ctx, apiBase, a.Username+":"+a.Password)
		if err != nil {
			return fmt.Errorf("basic auth check: %w", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("unauthorized: authentication required")
		}
		if resp2.StatusCode >= 400 {
			return fmt.Errorf("unexpected status %s", resp2.Status)
		}
		return nil
	case strings.HasPrefix(challenge, "bearer"):
		tokenURL, err := bearerTokenURL(resp.Header.Get("WWW-Authenticate"))
		if err != nil {
			return err
		}
		resp2, err := authGet(ctx, tokenURL, a.Username+":"+a.Password)
		if err != nil {
			return fmt.Errorf("token fetch: %w", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("unauthorized: incorrect username or password")
		}
		if resp2.StatusCode >= 400 {
			return fmt.Errorf("unexpected status %s", resp2.Status)
		}
		return nil
	default:
		// Unknown challenge; assume the registry knows what it wants and the
		// pull path will surface real errors if the creds are wrong.
		return nil
	}
}

// authGet issues a bounded GET, optionally with basic auth ("user:pass").
func authGet(ctx context.Context, url, basicAuth string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if basicAuth != "" {
		user, pass, _ := strings.Cut(basicAuth, ":")
		req.SetBasicAuth(user, pass)
	}
	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) //nolint:errcheck — drain for keep-alive
	return resp, nil
}

// bearerTokenURL extracts realm/service from a WWW-Authenticate header and
// builds the token endpoint URL. No scope is requested — like docker login
// itself, which just verifies the credentials; some registries (GHCR, ECR)
// reject token grants for unknown repository scopes with 403 while accepting
// a scope-less request.
func bearerTokenURL(header string) (string, error) {
	// "Bearer realm="https://auth.docker.io/token",service="registry.docker.io""
	params := map[string]string{}
	for _, kv := range strings.Split(strings.TrimPrefix(strings.TrimPrefix(header, "Bearer"), "bearer"), ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		params[strings.TrimSpace(strings.ToLower(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("bearer challenge without realm")
	}
	if svc := params["service"]; svc != "" {
		sep := "?"
		if strings.Contains(realm, "?") {
			sep = "&"
		}
		return realm + sep + "service=" + url.QueryEscape(svc), nil
	}
	return realm, nil
}
