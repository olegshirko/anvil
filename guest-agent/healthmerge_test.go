package main

import (
	"testing"
	"time"
)

func TestMergeHealthcheck(t *testing.T) {
	img := &dockerHealthcheck{Test: []string{"CMD", "true"}, Interval: 5, Retries: 7}
	if got := mergeHealthcheck(nil, img); got != img {
		t.Error("no container healthcheck must take the image's")
	}
	got := mergeHealthcheck(&dockerHealthcheck{Interval: 9}, img)
	if len(got.Test) != 2 || got.Interval != 9 || got.Retries != 7 {
		t.Errorf("partial override: %+v", got)
	}
	none := &dockerHealthcheck{Test: []string{"NONE"}}
	if mergeHealthcheck(none, img) != none {
		t.Error("NONE must disable the image healthcheck")
	}
}

func TestNextHealthDelay(t *testing.T) {
	began := time.Now()
	if d := nextHealthDelay(began, began.Add(time.Second), time.Minute, 5*time.Second, 30*time.Second, true); d != 5*time.Second {
		t.Errorf("inside start period: %v", d)
	}
	if d := nextHealthDelay(began, began.Add(2*time.Minute), time.Minute, 5*time.Second, 30*time.Second, true); d != 30*time.Second {
		t.Errorf("after start period: %v", d)
	}
	if d := nextHealthDelay(began, began.Add(time.Second), time.Minute, 5*time.Second, 30*time.Second, false); d != 30*time.Second {
		t.Errorf("already healthy: %v", d)
	}
}
