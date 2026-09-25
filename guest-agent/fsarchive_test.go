package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestTarTreeRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "app")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "sub"), 0o750))
	must(os.WriteFile(filepath.Join(src, "sub", "f"), []byte("payload"), 0o640))
	must(os.Symlink("/etc/passwd", filepath.Join(src, "abs-link")))
	must(os.Link(filepath.Join(src, "sub", "f"), filepath.Join(src, "hard")))

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	must(writeTarTree(tw, src, "app"))
	must(tw.Close())

	dst := t.TempDir()
	must(extractTar(bytes.NewReader(buf.Bytes()), dst))

	got, err := os.ReadFile(filepath.Join(dst, "app", "sub", "f"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("file: %q %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "app", "sub", "f")); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if l, err := os.Readlink(filepath.Join(dst, "app", "abs-link")); err != nil || l != "/etc/passwd" {
		t.Errorf("symlink stored as %q %v (must not be followed)", l, err)
	}
	a, _ := os.Stat(filepath.Join(dst, "app", "sub", "f"))
	b, _ := os.Stat(filepath.Join(dst, "app", "hard"))
	if a.Sys().(*syscall.Stat_t).Ino != b.Sys().(*syscall.Stat_t).Ino {
		t.Error("hard link not preserved")
	}

	// Export form: no prefix, the root entry itself omitted.
	var exp bytes.Buffer
	tw = tar.NewWriter(&exp)
	must(writeTarTree(tw, src, ""))
	must(tw.Close())
	tr := tar.NewReader(&exp)
	first, err := tr.Next()
	if err != nil || first.Name == "./" || first.Name == "" || first.Name[0] == '/' {
		t.Errorf("export first entry %+v %v", first, err)
	}
}

func TestExtractTarConfinesNames(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"../../escape", "/abs"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte("x"))
	}
	tw.Close()
	parent := t.TempDir()
	dst := filepath.Join(parent, "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(&buf, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escape")); err == nil {
		t.Error("../ entry escaped the destination")
	}
	for _, n := range []string{"escape", "abs"} {
		if _, err := os.Stat(filepath.Join(dst, n)); err != nil {
			t.Errorf("%s not extracted inside dst: %v", n, err)
		}
	}
}
