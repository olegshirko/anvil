package main

// Docker API read paths against the fake containerd gRPC server: the real
// containerd client, the real route table, the real handlers — only the
// daemon behind the unix socket is stubbed.

import (
	"encoding/json"
	"net/http"
	"testing"

	containerspb "github.com/containerd/containerd/api/services/containers/v1"
)

const fakeNamespace = "default"

// fixtureNS is the shared scenario: a running "web" container from nginx
// with a published port, a created "idle" container from alpine, two images.
func fixtureNS() []fakeContainerdOption {
	return []fakeContainerdOption{
		withContainer(&containerspb.Container{
			ID:    "c1-web",
			Image: "docker.io/library/nginx:latest",
			Labels: map[string]string{
				labelName:  "web",
				labelPorts: `[{"hostPort":8080,"containerPort":80,"protocol":"tcp","hostIP":"0.0.0.0"}]`,
			},
			Spec: ociSpecFor([]string{"nginx", "-g", "daemon off;"}, []string{"FOO=bar"}),
		}, runningTask(4242)),
		withContainer(&containerspb.Container{
			ID:     "c2-idle",
			Image:  "docker.io/library/alpine:latest",
			Labels: map[string]string{labelName: "idle"},
			Spec:   ociSpecFor([]string{"sleep", "60"}, nil),
		}, nil), // no task: created, not running
		withImage("docker.io/library/nginx:latest", "sha256:"+string64('a'), 1000),
		withImage("docker.io/library/alpine:latest", "sha256:"+string64('b'), 500),
	}
}

// string64 returns a 64-char string of the given byte (fake digests).
func string64(b byte) string {
	s := make([]byte, 64)
	for i := range s {
		s[i] = b
	}
	return string(s)
}

func TestContainersListAgainstFakeContainerd(t *testing.T) {
	startFakeContainerd(t, fakeNamespace, fixtureNS()...)
	srv := newTestAPIServer(t)

	// Default: running containers only.
	resp, err := http.Get(srv.URL + "/containers/json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var listed []dockerContainerSummary
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Names[0] != "/web" {
		t.Fatalf("running-only list = %+v", listed)
	}
	web := listed[0]
	if web.State != "running" {
		t.Errorf("web.State = %q, want running", web.State)
	}
	if web.Id != dockerID(fakeNamespace, "c1-web") {
		t.Errorf("web.Id = %q, want deterministic docker ID", web.Id)
	}
	if len(web.Ports) != 1 || web.Ports[0].PublicPort != 8080 || web.Ports[0].PrivatePort != 80 {
		t.Errorf("web.Ports = %+v", web.Ports)
	}

	// all=1 includes the created container.
	resp2, err := http.Get(srv.URL + "/containers/json?all=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var all []dockerContainerSummary
	if err := json.NewDecoder(resp2.Body).Decode(&all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all=1 returned %d containers, want 2", len(all))
	}
	for _, c := range all {
		if c.Names[0] == "/idle" && c.State != "created" {
			t.Errorf("idle.State = %q, want created", c.State)
		}
	}
}

func TestContainerInspectAgainstFakeContainerd(t *testing.T) {
	startFakeContainerd(t, fakeNamespace, fixtureNS()...)
	srv := newTestAPIServer(t)

	// By name.
	resp, err := http.Get(srv.URL + "/containers/web/json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var insp dockerContainerInspect
	if err := json.NewDecoder(resp.Body).Decode(&insp); err != nil {
		t.Fatal(err)
	}
	if insp.Id != dockerID(fakeNamespace, "c1-web") {
		t.Errorf("Id = %q", insp.Id)
	}
	if insp.Name != "/web" || !insp.State.Running || insp.State.Status != "running" {
		t.Errorf("name/state = %q %+v", insp.Name, insp.State)
	}
	if insp.State.Pid != 4242 {
		t.Errorf("Pid = %d, want 4242", insp.State.Pid)
	}
	// Image resolves through the image service (Get), not just info.Image.
	if insp.Image != "docker.io/library/nginx:latest" {
		t.Errorf("Image = %q", insp.Image)
	}
	if len(insp.Config.Env) == 0 || insp.Config.Env[0] != "FOO=bar" {
		t.Errorf("Config.Env = %+v", insp.Config.Env)
	}

	// By docker ID prefix.
	prefix := insp.Id[:12]
	resp2, err := http.Get(srv.URL + "/containers/" + prefix + "/json")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("inspect by prefix status = %d", resp2.StatusCode)
	}

	// Unknown name is a 404 with the JSON error shape.
	resp3, err := http.Get(srv.URL + "/containers/nosuch/json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown inspect status = %d, want 404", resp3.StatusCode)
	}
	decodeAPIError(t, resp3)
}

func TestImagesListAgainstFakeContainerd(t *testing.T) {
	startFakeContainerd(t, fakeNamespace, fixtureNS()...)
	srv := newTestAPIServer(t)

	resp, err := http.Get(srv.URL + "/images/json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var images []dockerImageSummary
	if err := json.NewDecoder(resp.Body).Decode(&images); err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 {
		t.Fatalf("got %d images, want 2", len(images))
	}
	found := map[string]bool{}
	for _, img := range images {
		found[img.RepoTags[0]] = true
		if len(img.Id) != 7+64 { // "sha256:" + 64 hex
			t.Errorf("image Id %q is not a full digest", img.Id)
		}
	}
	if !found["docker.io/library/nginx:latest"] || !found["docker.io/library/alpine:latest"] {
		t.Errorf("listed tags = %v", found)
	}
}

func TestSystemDFAgainstFakeContainerd(t *testing.T) {
	startFakeContainerd(t, fakeNamespace, fixtureNS()...)
	srv := newTestAPIServer(t)

	resp, err := http.Get(srv.URL + "/system/df")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var df struct {
		Images []struct {
			Id       string   `json:"Id"`
			RepoTags []string `json:"RepoTags"`
		} `json:"Images"`
		Containers []json.RawMessage `json:"Containers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&df); err != nil {
		t.Fatal(err)
	}
	if len(df.Images) != 2 {
		t.Fatalf("df lists %d images, want 2", len(df.Images))
	}
}
