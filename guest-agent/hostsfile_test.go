package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A hosts file rewrite never leaves the file empty or unchanged content
// rewritten: containers read it while the agent refreshes it.
func TestRewriteHostsManagedSectionInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	base := containerHostsContent("c1", nil, "192.168.65.254", "192.168.64.1")
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)

	rewriteHostsManagedSection(path, "") // nothing to add: no write at all
	if st, _ := os.Stat(path); !st.ModTime().Equal(old) {
		t.Error("unchanged hosts file was rewritten")
	}

	long := netHostsBegin + "\n10.10.1.2\tdb\n10.10.1.3\tcache\n" + netHostsEnd + "\n"
	rewriteHostsManagedSection(path, long)
	got, _ := os.ReadFile(path)
	if string(got) != base+long {
		t.Fatalf("after growing:\n%s", got)
	}
	short := netHostsBegin + "\n10.10.1.2\tdb\n" + netHostsEnd + "\n"
	rewriteHostsManagedSection(path, short)
	got, _ = os.ReadFile(path)
	if string(got) != base+short {
		t.Fatalf("after shrinking (stale tail left?):\n%s", got)
	}
}
