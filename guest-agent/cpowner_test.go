package main

import "testing"

func TestResolveContainerUserNumeric(t *testing.T) {
	for in, want := range map[string][2]int{"": {0, 0}, "4242": {4242, 0}, "4242:4343": {4242, 4343}} {
		uid, gid, err := resolveContainerUser(in)
		if err != nil || uid != want[0] || gid != want[1] {
			t.Errorf("%q: %d:%d %v, want %v", in, uid, gid, err, want)
		}
	}
	if _, _, err := resolveContainerUser("no-such-user-anvil"); err == nil {
		t.Error("unknown user name accepted")
	}
}
