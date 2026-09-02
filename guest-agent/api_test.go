package main

// HTTP-layer tests over the real route table (router.go) and handlers via
// httptest — no vsock and no containerd needed. Only endpoints that answer
// before touching the containerd client are covered here; the rest is
// exercised by the integration suite against a live VM.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newDockerAPIHandler())
	t.Cleanup(srv.Close)
	return srv
}

// decodeAPIError asserts the Docker error wire format: a JSON body with a
// single "message" field and an application/json content type. Hand-built
// bodies break on quotes in the message, so this is checked on every error.
func decodeAPIError(t *testing.T, resp *http.Response) string {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Message string `json:"message"`
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("error body is not valid JSON (%v): %q", err, raw)
	}
	if body.Message == "" {
		t.Errorf("error body has empty message: %q", raw)
	}
	return body.Message
}

func TestPingHeaders(t *testing.T) {
	srv := newTestAPIServer(t)
	resp, err := http.Get(srv.URL + "/_ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// The CLI reads server metadata from these instead of extra round-trips.
	for header, want := range map[string]string{
		"Api-Version":         dockerAPIVersion,
		"Ostype":              "linux",
		"Builder-Version":     "2",
		"BuildKit-Version":    "v0.32.2",
		"Docker-Experimental": "false",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "OK" {
		t.Errorf("ping body = %q, want OK", b)
	}
}

func TestAPIVersionPrefixStripped(t *testing.T) {
	srv := newTestAPIServer(t)
	resp, err := http.Get(srv.URL + "/v1.43/_ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("versioned ping status = %d", resp.StatusCode)
	}
}

func TestVersionEndpoint(t *testing.T) {
	srv := newTestAPIServer(t)
	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		ApiVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
		Os            string `json:"Os"`
		Arch          string `json:"Arch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.ApiVersion != dockerAPIVersion || v.MinAPIVersion != dockerMinAPIVersion {
		t.Errorf("api versions = %+v", v)
	}
	if v.Os != "linux" || v.Arch != "arm64" {
		t.Errorf("os/arch = %s/%s", v.Os, v.Arch)
	}
}

// postCreate sends POST /containers/create with the given body and platform
// query parameter.
func postCreate(t *testing.T, srv *httptest.Server, platform, body string) *http.Response {
	t.Helper()
	u := srv.URL + "/containers/create"
	if platform != "" {
		u += "?platform=" + platform
	}
	resp, err := http.Post(u, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCreateRejectsUnsupportedLogDriver(t *testing.T) {
	srv := newTestAPIServer(t)
	body := `{"Image":"alpine","HostConfig":{"LogConfig":{"Type":"syslog"}}}`
	resp := postCreate(t, srv, "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	msg := decodeAPIError(t, resp)
	// The message names the offending driver in quotes — it must arrive as
	// escaped, valid JSON (the regression writeJSONError fixed).
	if !strings.Contains(msg, `"syslog"`) || !strings.Contains(msg, "log driver") {
		t.Errorf("message = %q", msg)
	}
}

func TestCreateRejectsNonArm64Platform(t *testing.T) {
	srv := newTestAPIServer(t)
	resp := postCreate(t, srv, "linux/amd64", `{"Image":"alpine"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	msg := decodeAPIError(t, resp)
	if !strings.Contains(msg, "arm64") {
		t.Errorf("message = %q", msg)
	}
}

func TestCreateRejectsMalformedBody(t *testing.T) {
	srv := newTestAPIServer(t)
	resp := postCreate(t, srv, "", `{not json`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	decodeAPIError(t, resp)
}

func TestCreateRejectsUnsupportedHostConfigField(t *testing.T) {
	srv := newTestAPIServer(t)
	resp := postCreate(t, srv, "", `{"Image":"alpine","HostConfig":{"Runtime":"gvisor"}}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg := decodeAPIError(t, resp); !strings.Contains(msg, "Runtime") {
		t.Errorf("message = %q, want it to name Runtime", msg)
	}
}

func TestImageCreateMissingFromImage(t *testing.T) {
	srv := newTestAPIServer(t)
	resp, err := http.Post(srv.URL+"/images/create", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	decodeAPIError(t, resp)
}

func TestImageTagMissingRepo(t *testing.T) {
	srv := newTestAPIServer(t)
	resp, err := http.Post(srv.URL+"/images/myimg/tag", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	decodeAPIError(t, resp)
}

func TestImageDeleteGuardLiteralNames(t *testing.T) {
	srv := newTestAPIServer(t)
	// The *name wildcard must not swallow the literal sub-resources.
	resp, err := http.NewRequest(http.MethodDelete, srv.URL+"/images/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.DefaultClient.Do(resp)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE /images/json status = %d, want 404", r.StatusCode)
	}
}

func TestNetworkConnectMissingContainer(t *testing.T) {
	srv := newTestAPIServer(t)
	resp, err := http.Post(srv.URL+"/networks/bridge/connect", "application/json",
		bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	decodeAPIError(t, resp)
}

func TestVolumeCreateMalformedBody(t *testing.T) {
	srv := newTestAPIServer(t)
	resp, err := http.Post(srv.URL+"/volumes/create", "application/json",
		strings.NewReader(`nope`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	decodeAPIError(t, resp)
}

func TestContainerResizeInvalidDimensions(t *testing.T) {
	srv := newTestAPIServer(t)
	resp, err := http.Post(srv.URL+"/containers/abc/resize?h=0&w=0", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	decodeAPIError(t, resp)
}

func TestExecResizeUnknownExec(t *testing.T) {
	srv := newTestAPIServer(t)
	resp, err := http.Post(srv.URL+"/exec/nosuchexec/resize?h=10&w=10", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	decodeAPIError(t, resp)
}

func TestUnknownPathIs404(t *testing.T) {
	srv := newTestAPIServer(t)
	resp, err := http.Get(srv.URL + "/widgets/json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestConnectionCloseHeader(t *testing.T) {
	srv := newTestAPIServer(t)
	// net/http consumes hop-by-hop headers, so speak raw HTTP to the test
	// listener to see what actually goes on the wire.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /_ping HTTP/1.1\r\nHost: t\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	head := string(bytes.SplitN(raw, []byte("\r\n\r\n"), 2)[0])
	if !strings.Contains(head, "Connection: close") {
		t.Errorf("response headers missing Connection: close:\n%s", head)
	}
}
