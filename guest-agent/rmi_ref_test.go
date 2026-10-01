package main

import "testing"

func TestSingleImageReference(t *testing.T) {
	cases := []struct {
		records []string
		want    bool
	}{
		{[]string{"myapp:latest", "docker.io/library/myapp:latest"}, true},
		{[]string{"<none>@sha256:abc", "x:1"}, true},
		{[]string{"x:1", "x:2"}, false},
		{[]string{"x:1", "y:1"}, false},
		{[]string{"alpine:3", "docker.io/library/alpine@sha256:aa"}, true},
		{[]string{"alpine:3", "docker.io/library/busybox@sha256:aa"}, false},
		{[]string{"localhost:5000/x:1", "localhost:5000/x@sha256:aa"}, true},
		{[]string{"<none>@sha256:abc"}, true},
	}
	for _, c := range cases {
		if got := singleImageReference(c.records); got != c.want {
			t.Errorf("%v: got %v, want %v", c.records, got, c.want)
		}
	}
}
