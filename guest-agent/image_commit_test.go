package main

import (
	"reflect"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestApplyCommitChange(t *testing.T) {
	cfg := ocispec.ImageConfig{Env: []string{"PATH=/bin", "A=1"}, Cmd: []string{"sh"}}
	for _, ch := range []string{
		`CMD ["nginx", "-g", "daemon off;"]`,
		`ENTRYPOINT /docker-entrypoint.sh`,
		`ENV A=2 B="two words"`,
		`ENV LEGACY some value`,
		`LABEL maintainer=me version="1.0"`,
		`EXPOSE 80 53/udp`,
		`USER nobody`,
		`WORKDIR /srv`,
		`VOLUME ["/data", "/logs"]`,
		`STOPSIGNAL SIGQUIT`,
	} {
		if err := applyCommitChange(&cfg, ch); err != nil {
			t.Fatalf("%s: %v", ch, err)
		}
	}
	if !reflect.DeepEqual(cfg.Cmd, []string{"nginx", "-g", "daemon off;"}) {
		t.Errorf("cmd = %q", cfg.Cmd)
	}
	if !reflect.DeepEqual(cfg.Entrypoint, []string{"/bin/sh", "-c", "/docker-entrypoint.sh"}) {
		t.Errorf("entrypoint = %q", cfg.Entrypoint)
	}
	if !reflect.DeepEqual(cfg.Env, []string{"PATH=/bin", "A=2", "B=two words", "LEGACY=some value"}) {
		t.Errorf("env = %q", cfg.Env)
	}
	if cfg.Labels["maintainer"] != "me" || cfg.Labels["version"] != "1.0" {
		t.Errorf("labels = %v", cfg.Labels)
	}
	if _, ok := cfg.ExposedPorts["80/tcp"]; !ok {
		t.Errorf("exposed = %v", cfg.ExposedPorts)
	}
	if _, ok := cfg.ExposedPorts["53/udp"]; !ok {
		t.Errorf("exposed = %v", cfg.ExposedPorts)
	}
	if cfg.User != "nobody" || cfg.WorkingDir != "/srv" || cfg.StopSignal != "SIGQUIT" {
		t.Errorf("user/workdir/stopsignal = %q %q %q", cfg.User, cfg.WorkingDir, cfg.StopSignal)
	}
	if len(cfg.Volumes) != 2 {
		t.Errorf("volumes = %v", cfg.Volumes)
	}

	for _, bad := range []string{"RUN echo", "ONBUILD RUN x", "ENV", `LABEL "unterminated`} {
		if err := applyCommitChange(&cfg, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSplitEntrypoint(t *testing.T) {
	ep, cmd := splitEntrypoint([]string{"/entry.sh", "postgres"}, []string{"/entry.sh"})
	if !reflect.DeepEqual(ep, []string{"/entry.sh"}) || !reflect.DeepEqual(cmd, []string{"postgres"}) {
		t.Errorf("split = %q %q", ep, cmd)
	}
	ep, cmd = splitEntrypoint([]string{"sh", "-c", "x"}, []string{"/entry.sh"})
	if ep != nil || !reflect.DeepEqual(cmd, []string{"sh", "-c", "x"}) {
		t.Errorf("no-prefix split = %q %q", ep, cmd)
	}
}

func TestCommitImageName(t *testing.T) {
	d := digest.FromString("x")
	cases := []struct{ repo, tag, want string }{
		{"myapp", "", "docker.io/library/myapp:latest"},
		{"me/app", "v2", "docker.io/me/app:v2"},
		{"localhost:5000/app", "t", "localhost:5000/app:t"},
		{"", "", "<none>@" + d.String()},
	}
	for _, tc := range cases {
		if got := commitImageName(tc.repo, tc.tag, d); got != tc.want {
			t.Errorf("commitImageName(%q, %q) = %q, want %q", tc.repo, tc.tag, got, tc.want)
		}
	}
}
