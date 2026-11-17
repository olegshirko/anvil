//go:build linux

package main

// AF_PACKET ARP probing and the lease-event monitor (see netlease.go for the
// wire format and rationale). Linux-only: the agent runs inside the VM as PID
// 1 with CAP_NET_RAW; the rest of the package still builds and tests on the
// host.

import (
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// leasePollInterval bounds how long after a renew the duplicate probe
	// runs; a stat on tmpfs twice a second is negligible.
	leasePollInterval = 2 * time.Second
	// leaseProbeInterval catches an address taken between lease events (the
	// usual outage: the NAT hands our address to a new VM without telling
	// us), so a conflict is reported even when no renewal is due.
	leaseProbeInterval = 60 * time.Second
	// arpProbeTimeout is how long a probe waits for a defending reply. ARP
	// on a virtual switch is answered in microseconds; the margin is for a
	// loaded guest.
	arpProbeTimeout = 500 * time.Millisecond
)

// startLeaseMonitor probes the leased address after boot and on every lease
// event. Detection only; it never reconfigures the interface.
func startLeaseMonitor() {
	goSafe("lease-monitor", func() {
		var (
			lastEvent time.Time
			lastProbe time.Time
			lastClaim string
		)
		probe := func() {
			lastProbe = time.Now()
			data, err := os.ReadFile(leaseFilePath)
			if err != nil {
				return // no lease: nothing to defend
			}
			lease, err := parseLease(data)
			if err != nil {
				debugLog("lease: %v", err)
				return
			}
			claimer, err := arpProbe(lease.IP)
			if err != nil {
				debugLog("lease probe %s: %v", lease.IP, err)
				return
			}
			if claimer == nil {
				lastClaim = "" // conflict gone; a new one logs again
				return
			}
			claim := lease.IP.String() + " " + claimer.String()
			if claim == lastClaim {
				return // already reported
			}
			lastClaim = claim
			consoleLog("[net] duplicate address %s claimed by %s", lease.IP, claimer)
		}
		for {
			// The event script rewrites the file with an atomic rename, so
			// its mtime changes exactly on a bound/renew; a rename cannot be
			// missed the way an inotify watch on the old inode could.
			if st, err := os.Stat(leaseFilePath); err == nil {
				if st.ModTime().After(lastEvent) {
					lastEvent = st.ModTime()
					probe()
				} else if time.Since(lastProbe) >= leaseProbeInterval {
					probe()
				}
			}
			time.Sleep(leasePollInterval)
		}
	})
}

// arpProbe sends one ARP probe for target on eth0 and returns the MAC of a
// host that answered for it, or nil when the address is unclaimed.
func arpProbe(target net.IP) (net.HardwareAddr, error) {
	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		return nil, err
	}
	if len(iface.HardwareAddr) != 6 {
		return nil, fmt.Errorf("eth0 has no usable MAC")
	}
	frame, err := arpProbeFrame(iface.HardwareAddr, target)
	if err != nil {
		return nil, err
	}
	// A raw AF_PACKET socket bound to eth0 with the ARP protocol: the kernel
	// delivers ARP frames received on that interface only.
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return nil, fmt.Errorf("af_packet: %w", err)
	}
	defer unix.Close(fd) //nolint:errcheck — probe exit path
	bind := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP), Ifindex: iface.Index}
	if err := unix.Bind(fd, bind); err != nil {
		return nil, fmt.Errorf("bind af_packet: %w", err)
	}
	var broadcast [8]uint8
	for i := range broadcast {
		broadcast[i] = 0xff
	}
	dst := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ARP),
		Ifindex:  iface.Index,
		Halen:    6,
		Addr:     broadcast,
	}
	if err := unix.Sendto(fd, frame, 0, dst); err != nil {
		return nil, fmt.Errorf("send arp probe: %w", err)
	}
	// Read replies until the deadline. Stray ARP traffic on the segment is
	// filtered out by arpClaimSource, not by the socket.
	deadline := time.Now().Add(arpProbeTimeout)
	timeout := unix.NsecToTimeval(int64(arpProbeTimeout))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		return nil, fmt.Errorf("af_packet timeout: %w", err)
	}
	buf := make([]byte, 2048)
	for time.Now().Before(deadline) {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return nil, fmt.Errorf("recv arp reply: %w", err)
		}
		if mac, ok := arpClaimSource(buf[:n], iface.HardwareAddr, target); ok {
			return mac, nil
		}
	}
	return nil, nil
}

// htons converts a protocol number to network byte order for AF_PACKET. The
// agent only runs on little-endian Linux (arm64, amd64).
func htons(v uint16) uint16 {
	return v<<8 | v>>8
}

var (
	consoleMu    sync.Mutex
	consoleFile  *os.File
	consoleTried bool
)

// mirrorConsole appends line to /dev/console unless the agent's stdout is
// already the console (normal mode), which would print it twice.
func mirrorConsole(line string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if !consoleTried {
		consoleTried = true
		if !stdoutIsConsole() {
			if f, err := os.OpenFile("/dev/console", os.O_WRONLY|os.O_APPEND, 0); err == nil {
				consoleFile = f
			}
		}
	}
	if consoleFile != nil {
		fmt.Fprintln(consoleFile, line) //nolint:errcheck — console output is best effort
	}
}

// stdoutIsConsole reports whether stdout refers to the VM console character
// device: stage2 leaves it there normally and redirects it to a file on the
// virtiofs share in debug mode.
func stdoutIsConsole() bool {
	var out, con unix.Stat_t
	if err := unix.Fstat(int(os.Stdout.Fd()), &out); err != nil {
		return false
	}
	if err := unix.Stat("/dev/console", &con); err != nil {
		return false
	}
	return out.Mode&unix.S_IFMT == unix.S_IFCHR &&
		con.Mode&unix.S_IFMT == unix.S_IFCHR &&
		out.Rdev == con.Rdev
}
