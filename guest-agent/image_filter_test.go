package main

import (
	"reflect"
	"testing"
)

func TestFilterImageSummariesReference(t *testing.T) {
	imgs := []dockerImageSummary{
		{Id: "1", RepoTags: []string{"docker.io/library/busybox:latest", "docker.io/library/busybox:musl"}},
		{Id: "2", RepoTags: []string{"docker.io/library/alpine:3.20"}},
		{Id: "3", RepoTags: []string{"docker.io/me/app:1"}},
		{Id: "4", RepoTags: []string{"ghcr.io/org/tool:v2"}, Labels: map[string]string{"team": "x"}},
		{Id: "5"},
	}
	ids := func(out []dockerImageSummary) []string {
		var r []string
		for _, i := range out {
			r = append(r, i.Id)
		}
		return r
	}
	cases := []struct {
		filters string
		want    []string
	}{
		{`{"reference":["busybox"]}`, []string{"1"}},
		{`{"reference":["busybox:musl"]}`, []string{"1"}},
		{`{"reference":["alp*"]}`, []string{"2"}},
		{`{"reference":["*/app"]}`, []string{"3"}},
		{`{"reference":["ghcr.io/org/tool"]}`, []string{"4"}},
		{`{"dangling":["true"]}`, []string{"5"}},
		{`{"dangling":["false"]}`, []string{"1", "2", "3", "4"}},
		{`{"label":["team=x"]}`, []string{"4"}},
		{``, []string{"1", "2", "3", "4", "5"}},
	}
	for _, tc := range cases {
		if got := ids(filterImageSummaries(imgs, parseDockerFilters(tc.filters))); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.filters, got, tc.want)
		}
	}
	// A tag filter narrows RepoTags to the matching tag.
	got := filterImageSummaries(imgs, parseDockerFilters(`{"reference":["busybox:musl"]}`))
	if len(got) != 1 || !reflect.DeepEqual(got[0].RepoTags, []string{"docker.io/library/busybox:musl"}) {
		t.Errorf("RepoTags not narrowed: %+v", got)
	}
}
