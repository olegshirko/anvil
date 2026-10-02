package main

import (
	"reflect"
	"testing"
)

func TestContainerDomains(t *testing.T) {
	got := containerDomains("/My_App-1", map[string]string{
		"com.docker.compose.service": "web", "com.docker.compose.project": "Shop"})
	if want := []string{"my-app-1", "web.shop"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestContainerHTTPPort(t *testing.T) {
	cases := []struct {
		exposed []string
		labels  map[string]string
		want    int
	}{
		{nil, nil, 80},
		{[]string{"5432/tcp"}, nil, 5432},
		{[]string{"8080/tcp", "80/tcp"}, nil, 80},
		{[]string{"9000/tcp", "3000/tcp", "53/udp"}, nil, 3000},
		{[]string{"80/tcp"}, map[string]string{domainPortLabel: "5173"}, 5173},
	}
	for _, c := range cases {
		if got := containerHTTPPort(c.exposed, c.labels); got != c.want {
			t.Errorf("%v %v: %d, want %d", c.exposed, c.labels, got, c.want)
		}
	}
}
