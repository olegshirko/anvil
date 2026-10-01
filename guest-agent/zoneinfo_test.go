package main

import (
	"testing"
	"time"
)

func TestUTCTZifLoads(t *testing.T) {
	loc, err := time.LoadLocationFromTZData("UTC", utcTZif())
	if err != nil {
		t.Fatal(err)
	}
	name, off := time.Date(2026, 7, 1, 12, 0, 0, 0, loc).Zone()
	if name != "UTC" || off != 0 {
		t.Fatalf("zone %s %d", name, off)
	}
}
