package main

import "testing"

func TestIsolationOnTop(t *testing.T) {
	c := `anvil-internal br1"`
	on := "-P FORWARD ACCEPT\n" +
		`-A FORWARD -i br1 ! -o br1 -m comment --comment "anvil-internal br1" -j DROP` + "\n" +
		`-A FORWARD -o br1 ! -i br1 -m comment --comment "anvil-internal br1" -j DROP` + "\n" +
		"-A FORWARD -m comment --comment \"CNI firewall plugin rules\" -j CNI-FORWARD\n"
	if !isolationOnTop(on, c) {
		t.Fatal("rules on top not recognised")
	}
	below := "-P FORWARD ACCEPT\n" +
		"-A FORWARD -m comment --comment \"CNI firewall plugin rules\" -j CNI-FORWARD\n" +
		`-A FORWARD -i br1 ! -o br1 -m comment --comment "anvil-internal br1" -j DROP` + "\n" +
		`-A FORWARD -o br1 ! -i br1 -m comment --comment "anvil-internal br1" -j DROP` + "\n"
	if isolationOnTop(below, c) {
		t.Fatal("rules below CNI-FORWARD taken as on top")
	}
	other := `-A FORWARD -i br10 ! -o br10 -m comment --comment "anvil-internal br10" -j DROP` + "\n" +
		`-A FORWARD -o br10 ! -i br10 -m comment --comment "anvil-internal br10" -j DROP` + "\n"
	if isolationOnTop(other, c) {
		t.Fatal("another bridge's rules matched")
	}
}
