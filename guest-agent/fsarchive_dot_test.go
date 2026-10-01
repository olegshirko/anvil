package main

import "testing"

func TestCpBaseName(t *testing.T) {
	for in, want := range map[string]string{
		"/etc/.": ".", "/etc/./": ".", "etc/.": ".", ".": ".", "/etc": "etc", "/etc/": "etc", "/": "/", "/a/b/./c": "c",
	} {
		if got := cpBaseName(in); got != want {
			t.Errorf("cpBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}
