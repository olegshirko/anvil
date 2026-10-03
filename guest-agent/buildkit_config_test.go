package main

import (
	"strings"
	"testing"
)

func TestBuildkitdConfigWithCacheCap(t *testing.T) {
	base := "[worker.oci]\n  enabled = false\n\n[worker.containerd]\n  enabled = true\n  namespace = \"default\"\n"
	got := buildkitdConfigWithCacheCap(base, 20)
	want := "[worker.containerd]\n  gc = true\n  maxUsedSpace = \"20GB\"\n  enabled = true\n"
	if !strings.Contains(got, want) {
		t.Errorf("cap not in the containerd worker section:\n%s", got)
	}
	if strings.Count(got, "maxUsedSpace") != 1 {
		t.Errorf("cap added more than once:\n%s", got)
	}
	if buildkitdConfigWithCacheCap(base, 0) != base {
		t.Error("0 must keep buildkit's own policy")
	}
}
