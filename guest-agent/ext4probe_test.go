package main

import (
	"bytes"
	"testing"
)

func TestHasExt4Magic(t *testing.T) {
	img := make([]byte, 4096)
	if ok, err := hasExt4Magic(bytes.NewReader(img)); err != nil || ok {
		t.Fatalf("zeroed disk: %v %v", ok, err)
	}
	img[ext4MagicOffset], img[ext4MagicOffset+1] = 0x53, 0xEF
	if ok, err := hasExt4Magic(bytes.NewReader(img)); err != nil || !ok {
		t.Fatalf("ext4 superblock: %v %v", ok, err)
	}
	if _, err := hasExt4Magic(bytes.NewReader(img[:100])); err == nil {
		t.Fatal("short device read must be an error, not \"no filesystem\"")
	}
}
