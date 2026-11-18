package main

// Reinitialization of the guest's own network (control command `net_reset`).
//
// A VM restored from a Virtualization.framework snapshot can come back with
// its virtio-net device unable to pass frames: the Mac's ARP entry for the
// guest turns "(incomplete)", published ports answer nothing, while the guest
// still holds its address and the vsock control channel keeps working. The
// host reattaches the vmnet NAT attachment (NATRecovery); this is the guest
// half of the recovery: bring the interface's driver down and up, which
// resets the virtio queues, then make DHCP apply a lease again because the
// reattached NAT may no longer honour the one the client holds.
//
// Bounded and conservative: the old address is only dropped when an ordinary
// renewal did not apply within netResetRenewWait, and every external command
// has a timeout so a wedged driver cannot hang the control request the host
// is waiting on.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	netResetIface = "eth0"
	// netResetRenewWait bounds a forced renewal before a full discover is
	// attempted. The client retries a renewal every -T seconds (2) and the
	// link needs a moment for carrier after the up, so the first attempt
	// after the bounce often fails; six seconds cut a renewal that landed
	// just after the window short and always forced a full discover.
	netResetRenewWait = 12 * time.Second
	// netResetDiscoverWait bounds the one-shot discover.
	netResetDiscoverWait = 10 * time.Second
	// netResetStepTimeout bounds one ip/killall/ifconfig call.
	netResetStepTimeout = 3 * time.Second
	// netResetLeaseSlack covers the script's write racing our stat after the
	// one-shot udhcpc has already exited successfully.
	netResetLeaseSlack = 2 * time.Second
)

// netResetCommands is the link half of the reset, in order. Pure so the
// sequence is unit-tested on the host: the driver must see down before up.
func netResetCommands(iface string) []string {
	return []string{
		"ip link set " + iface + " down",
		"ip link set " + iface + " up",
	}
}

// resetGuestNetwork is the control-channel `net_reset` command.
func resetGuestNetwork() Response {
	started := time.Now()
	var out strings.Builder
	for _, cmd := range netResetCommands(netResetIface) {
		text, err := runNetResetCommand(cmd, netResetStepTimeout)
		if text != "" {
			out.WriteString(text + "\n")
		}
		if err != nil {
			return netResetFailure(cmd, err, &out)
		}
	}
	consoleLog("[net] reset: brought %s down and up", netResetIface)

	// Force the resident client to renew immediately. A renewal rewrites the
	// lease file (bound and renew take the same script branch), so a changed
	// mtime means the address, route and resolv.conf were applied.
	if _, err := runNetResetCommand("killall -USR1 udhcpc", netResetStepTimeout); err == nil {
		if waitLeaseApplied(started, netResetRenewWait) {
			return netResetDone(&out)
		}
	}

	// The renewal did not apply: drop the stale lease and discover again.
	// stage2's udhcpc loop restarts a resident client after the kill; the
	// one-shot below guarantees a discover even if that loop is finished.
	if text, err := runNetResetCommand("killall -9 udhcpc", netResetStepTimeout); err == nil && text != "" {
		out.WriteString(text + "\n")
	}
	if err := os.Remove(leaseFilePath); err != nil && !os.IsNotExist(err) {
		return netResetFailure("remove the lease file", err, &out)
	}
	// The event script zeroes the address on a lost lease; do the same so
	// the client cannot keep answering ARP for an address the NAT has
	// forgotten.
	if text, err := runNetResetCommand("ifconfig "+netResetIface+" 0.0.0.0 up", netResetStepTimeout); err != nil {
		return netResetFailure("clear the address", err, &out)
	} else if text != "" {
		out.WriteString(text + "\n")
	}

	ctx, cancel := context.WithTimeout(context.Background(), netResetDiscoverWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "udhcpc", "-i", netResetIface, "-n", "-q",
		"-t", "3", "-T", "2", "-s", "/etc/udhcpc-script")
	discover, err := cmd.CombinedOutput()
	if text := strings.TrimSpace(string(discover)); text != "" {
		out.WriteString(text + "\n")
	}
	if err != nil {
		return netResetFailure("dhcp discover", err, &out)
	}
	if !waitLeaseApplied(started, netResetLeaseSlack) {
		return netResetFailure("dhcp discover", fmt.Errorf("no lease file after a successful discover"), &out)
	}
	return netResetDone(&out)
}

// netResetDone reports the lease the reset ended with.
func netResetDone(out *strings.Builder) Response {
	data, err := os.ReadFile(leaseFilePath)
	if err != nil {
		return netResetFailure("read the lease", err, out)
	}
	lease, err := parseLease(data)
	if err != nil {
		return netResetFailure("parse the lease", err, out)
	}
	consoleLog("[net] reset: lease %s", lease.IP)
	return Response{
		Status: "ok",
		Stdout: out.String() + fmt.Sprintf("lease %s\n", lease.IP),
	}
}

func netResetFailure(what string, err error, out *strings.Builder) Response {
	msg := fmt.Sprintf("%s: %v", what, err)
	consoleLog("[net] reset failed: %s", msg)
	return Response{Error: msg, Stdout: out.String(), ExitCode: 1}
}

// waitLeaseApplied reports whether the lease file was rewritten at or after
// since, polling up to timeout.
func waitLeaseApplied(since time.Time, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if leaseFileChangedAfter(leaseFilePath, since) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// leaseFileChangedAfter reports whether path exists and was last written at
// or after since. The udhcpc script rewrites it on every bound and renew, so
// a change means DHCP applied a lease.
func leaseFileChangedAfter(path string, since time.Time) bool {
	st, err := os.Stat(path)
	return err == nil && !st.ModTime().Before(since)
}

// runNetResetCommand runs one shell command with a timeout and returns its
// trimmed combined output.
func runNetResetCommand(command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/sh", "-c", command).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
