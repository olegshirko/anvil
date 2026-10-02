package main

import (
	"reflect"
	"testing"
)

func TestMinimalWatchPaths(t *testing.T) {
	got := minimalWatchPaths([]string{"/Users/a/b", "/Users/a", "/Users/ab", "/Users/a", "/private/tmp/x"})
	want := []string{"/Users/a", "/Users/ab", "/private/tmp/x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOnMacShare(t *testing.T) {
	for p, want := range map[string]bool{
		"/Users/me/src": true, "/private/var/folders/x": true, "/var/lib/x": false, "/Usersx": false,
	} {
		if onMacShare(p) != want {
			t.Errorf("%s: %v", p, !want)
		}
	}
}
