package inotify

import (
	"reflect"
	"strconv"
	"testing"
)

func TestPruneSubpaths(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{
			args: []string{"/", "/user", "/user/someone", "/a", "/a/ee", "/a/bb"},
			want: []string{"/"},
		},
		{
			args: []string{"/someone", "/user", "/user/someone", "/a", "/a/ee", "/a/bb", "/a"},
			want: []string{"/a", "/someone", "/user"},
		},
		{
			args: []string{"/someone", "/user/anvil/projects/myworks", "/user/anvil/projects", "/user/anvil/projects/myworks", "/user/anvil/projects", "/someone"},
			want: []string{"/someone", "/user/anvil/projects"},
		},
		{
			args: []string{"/someone", "/user/anvil/projects/myworks", "/user/anvil/projects"},
			want: []string{"/someone", "/user/anvil/projects"},
		},
		{
			args: []string{"/user/anvil/projects"},
			want: []string{"/user/anvil/projects"},
		},
	}
	for i, tt := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if got := pruneSubpaths(tt.args); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("pruneSubpaths() = %v, want %v", got, tt.want)
			}
		})
	}
}
