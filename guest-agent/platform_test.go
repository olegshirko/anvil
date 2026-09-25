package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func withRosetta(t *testing.T, on bool) {
	t.Helper()
	orig := rosettaActive
	rosettaActive = func() bool { return on }
	t.Cleanup(func() { rosettaActive = orig })
}

func TestResolveRequestedPlatform(t *testing.T) {
	cases := []struct {
		in      string
		rosetta bool
		want    string
		errPart string
	}{
		{"", false, "", ""},
		{"linux", false, "", ""},
		{"linux/arm64", false, "", ""},
		{"linux/arm64/v8", true, "linux/arm64/v8", ""}, // explicit arm64 must not fall back to amd64
		{"linux/arm64", true, "linux/arm64/v8", ""},
		{"linux/amd64", false, "", "ANVIL_ROSETTA=1"},
		{"linux/amd64", true, "linux/amd64", ""},
		{"linux/x86_64", true, "linux/amd64", ""},
		{"linux/amd64/v2", true, "", "linux/amd64/v2"},
		{"linux/386", true, "", "linux/386"},
		{"windows/amd64", true, "", "windows/amd64"},
	}
	for _, tc := range cases {
		withRosetta(t, tc.rosetta)
		got, err := resolveRequestedPlatform(tc.in)
		if tc.errPart != "" {
			if err == nil || !strings.Contains(err.Error(), tc.errPart) {
				t.Errorf("%q (rosetta=%v): err = %v, want mention of %q", tc.in, tc.rosetta, err, tc.errPart)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q (rosetta=%v) = %q, %v; want %q", tc.in, tc.rosetta, got, err, tc.want)
		}
	}
}

func TestValidateCreatePlatformWithRosetta(t *testing.T) {
	withRosetta(t, true)
	r := httptest.NewRequest("POST", "/v1.43/containers/create?platform=linux%2Famd64", nil)
	if err := validateCreatePlatform(r); err != nil {
		t.Errorf("linux/amd64 refused with Rosetta: %v", err)
	}
}

func TestDefaultPlatformMatcher(t *testing.T) {
	arm := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	amd := ocispec.Platform{OS: "linux", Architecture: "amd64"}

	withRosetta(t, false)
	m := defaultPlatformMatcher()
	if !m.Match(arm) || m.Match(amd) {
		t.Errorf("without Rosetta: arm64=%v amd64=%v, want true/false", m.Match(arm), m.Match(amd))
	}

	withRosetta(t, true)
	m = defaultPlatformMatcher()
	if !m.Match(arm) || !m.Match(amd) {
		t.Errorf("with Rosetta both must match: arm64=%v amd64=%v", m.Match(arm), m.Match(amd))
	}
	if !m.Less(arm, amd) || m.Less(amd, arm) {
		t.Error("arm64 must be preferred over amd64")
	}

	strict := platformMatcher("linux/amd64")
	if strict.Match(arm) || !strict.Match(amd) {
		t.Errorf("explicit amd64: arm64=%v amd64=%v", strict.Match(arm), strict.Match(amd))
	}
}
