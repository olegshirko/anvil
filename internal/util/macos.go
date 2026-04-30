package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"anvil/internal/cli"

	"github.com/coreos/go-semver/semver"
	"github.com/sirupsen/logrus"
)

// IsMacOS reports whether the host operating system is macOS.
func IsMacOS() bool { return runtime.GOOS == "darwin" }

// AppleSiliconAndModernOS reports whether the device is Apple Silicon running macOS 13+.
func AppleSiliconAndModernOS() bool {
	return runtime.GOARCH == "arm64" && AtLeastVentura()
}

// AtLeastVentura reports whether the host is running macOS 13 (Ventura) or newer.
func AtLeastVentura() bool { return hostMeetsVersion("13.0.0") }

// AtLeastSequoia reports whether the host is running macOS 15 (Sequoia) or newer.
func AtLeastSequoia() bool { return hostMeetsVersion("15.0.0") }

// SupportsNestedVirt reports whether the device supports nested virtualization.
func SupportsNestedVirt() bool {
	return (HasAppleSiliconGen(3) || HasAppleSiliconGen(4)) && AtLeastSequoia()
}

// hostMeetsVersion checks the host macOS version against the given minimum.
func hostMeetsVersion(min string) bool {
	if !IsMacOS() {
		return false
	}
	hostVer, err := readHostVersion()
	if err != nil {
		logrus.Warnln("cannot detect macOS version:", err)
		return false
	}
	required, err := semver.NewVersion(min)
	if err != nil {
		logrus.Warnln("invalid minimum version:", err)
		return false
	}
	return required.Compare(*hostVer) <= 0
}

// HasAppleSiliconGen reports whether the host uses an Apple Silicon M{x} chip.
func HasAppleSiliconGen(gen int) bool {
	var profile struct {
		SPHardwareDataType []struct {
			ChipType string `json:"chip_type"`
		} `json:"SPHardwareDataType"`
	}

	var buf bytes.Buffer
	cmd := cli.Exec("system_profiler", "-json", "SPHardwareDataType").Cmd()
	cmd.Stdout = &buf

	if err := cmd.Run(); err != nil {
		logrus.Tracef("cannot query hardware profile: %v", err)
		return false
	}
	if err := json.NewDecoder(&buf).Decode(&profile); err != nil {
		logrus.Tracef("cannot decode hardware profile: %v", err)
		return false
	}
	if len(profile.SPHardwareDataType) == 0 {
		return false
	}

	chip := strings.ToUpper(profile.SPHardwareDataType[0].ChipType)
	return strings.Contains(chip, fmt.Sprintf("M%d", gen))
}

// IsRosettaActive reports whether the Rosetta translation process is running.
func IsRosettaActive() bool {
	if !IsMacOS() {
		return false
	}
	cmd := cli.Exec("pgrep", "oahd").Cmd()
	cmd.Stderr = nil
	cmd.Stdout = nil
	return cmd.Run() == nil
}

// readHostVersion returns the host's macOS product version as a semver.
func readHostVersion() (*semver.Version, error) {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return nil, fmt.Errorf("sw_vers failed: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	// ensure at least major.minor.patch
	for strings.Count(raw, ".") < 2 {
		raw += ".0"
	}
	v, err := semver.NewVersion(raw)
	if err != nil {
		return nil, fmt.Errorf("cannot parse macOS version %q: %w", raw, err)
	}
	return v, nil
}
