package main

// Lease awareness and duplicate-address detection.
//
// The initramfs udhcpc event script writes the current lease to
// /run/anvil-lease ("<ip> <netmask> <router>") and prints it to the console
// on every bound/renew/deconfig. The agent polls that file and ARP-probes the
// leased address: a reply from another MAC means a second VM was handed the
// same address (the vmnet DHCP server does this once a lease is not renewed),
// and both guests then answer ARP for it.
//
// Detection only: the agent never reconfigures the interface itself. The
// udhcpc event script applies whatever lease the vmnet DHCP server hands out
// (a renewal can move the address; the host's port forwarder follows it from
// every pushed state instead of caching it), and the CNI bridges keep their
// own 10.10.x subnets, which no lease move touches.
//
// The frame code here is pure Go and unit-tested on the host; the AF_PACKET
// socket and /dev/console mirroring live in netlease_linux.go.

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
)

// ARP wire constants (RFC 826; the probe form is RFC 5227 section 2.1.1).
const (
	arpEtherType        = 0x0806
	arpHardwareEthernet = 1
	arpProtocolIPv4     = 0x0800
	arpOpRequest        = 1
	arpOpReply          = 2
	// ethernetHeaderLen + arpPacketLen for Ethernet/IPv4 ARP.
	arpFrameLen = 14 + 28
)

// netLease is the guest's current DHCP lease, as the udhcpc script leaves it
// in /run/anvil-lease.
type netLease struct {
	IP     net.IP
	Mask   net.IPMask
	Router net.IP
}

// leaseFilePath is written by the initramfs udhcpc event script on every
// bound/renew and removed on deconfig/nak/leasefail. /run is the tmpfs runtime
// directory stage2 already uses for its markers. Portable (not linux-only):
// the `status` reply reports it and the darwin build compiles this file.
// var, not const: the status test points it at a temp file.
var leaseFilePath = "/run/anvil-lease"

// currentGuestIP returns the leased IPv4 address, or "" when there is no
// lease file (boot before DHCP, or a lost lease). It is what `status` reports
// to the host for the duplicate-address check.
func currentGuestIP() string {
	data, err := os.ReadFile(leaseFilePath)
	if err != nil {
		return ""
	}
	lease, err := parseLease(data)
	if err != nil {
		return ""
	}
	return lease.IP.String()
}

// parseLease parses "<ip> <netmask> <router>"; the router is optional (a
// lease can carry no default gateway). Anything else is treated as corrupt
// rather than guessed at: probing the wrong address would report conflicts
// that are not there.
func parseLease(data []byte) (netLease, error) {
	f := strings.Fields(string(data))
	if len(f) < 2 || len(f) > 3 {
		return netLease{}, fmt.Errorf("lease: want 2 or 3 fields, got %d", len(f))
	}
	ip := net.ParseIP(f[0]).To4()
	if ip == nil {
		return netLease{}, fmt.Errorf("lease: bad address %q", f[0])
	}
	mask := net.ParseIP(f[1]).To4()
	if mask == nil {
		return netLease{}, fmt.Errorf("lease: bad netmask %q", f[1])
	}
	l := netLease{IP: ip, Mask: net.IPMask(mask)}
	if len(f) == 3 {
		r := net.ParseIP(f[2]).To4()
		if r == nil {
			return netLease{}, fmt.Errorf("lease: bad router %q", f[2])
		}
		l.Router = r
	}
	return l, nil
}

// arpProbeFrame builds the Ethernet frame of an ARP probe for target
// (RFC 5227): broadcast destination, sender protocol address 0.0.0.0 and
// target hardware address zero. A host that already owns target must defend
// it with a reply, while the zero sender keeps the probe from touching any
// peer's ARP cache.
func arpProbeFrame(srcMAC net.HardwareAddr, target net.IP) ([]byte, error) {
	if len(srcMAC) != 6 {
		return nil, fmt.Errorf("arp probe: bad source MAC %v", srcMAC)
	}
	tip := target.To4()
	if tip == nil {
		return nil, fmt.Errorf("arp probe: %v is not IPv4", target)
	}
	frame := make([]byte, arpFrameLen)
	// Ethernet header: broadcast destination, our source.
	for i := 0; i < 6; i++ {
		frame[i] = 0xff
	}
	copy(frame[6:12], srcMAC)
	binary.BigEndian.PutUint16(frame[12:14], arpEtherType)
	// ARP packet: Ethernet/IPv4, 6-byte hardware and 4-byte protocol
	// addresses, request.
	binary.BigEndian.PutUint16(frame[14:16], arpHardwareEthernet)
	binary.BigEndian.PutUint16(frame[16:18], arpProtocolIPv4)
	frame[18] = 6
	frame[19] = 4
	binary.BigEndian.PutUint16(frame[20:22], arpOpRequest)
	copy(frame[22:28], srcMAC) // sender hardware
	// frame[28:32] sender protocol stays 0.0.0.0 (the probe).
	// frame[32:38] target hardware stays zero.
	copy(frame[38:42], tip) // target protocol
	return frame, nil
}

// arpClaimSource reports the MAC of a host claiming target in frame: an ARP
// packet whose sender protocol address is target, sent by a MAC other than
// ours. Both a reply to the probe and a gratuitous request announcing the
// address count; a plain ARP request asking "who has target" does not (its
// sender address is the asker's own, not target).
func arpClaimSource(frame []byte, ourMAC net.HardwareAddr, target net.IP) (net.HardwareAddr, bool) {
	if len(frame) < arpFrameLen {
		return nil, false
	}
	if binary.BigEndian.Uint16(frame[12:14]) != arpEtherType ||
		binary.BigEndian.Uint16(frame[14:16]) != arpHardwareEthernet ||
		binary.BigEndian.Uint16(frame[16:18]) != arpProtocolIPv4 ||
		frame[18] != 6 || frame[19] != 4 {
		return nil, false
	}
	switch binary.BigEndian.Uint16(frame[20:22]) {
	case arpOpRequest, arpOpReply:
	default:
		return nil, false
	}
	tip := target.To4()
	if tip == nil || !net.IP(frame[28:32]).Equal(tip) {
		return nil, false // sender address is not the address we asked about
	}
	src := net.HardwareAddr(frame[22:28])
	if src.String() == ourMAC.String() {
		return nil, false // the guest's own kernel or our echoed probe
	}
	return src, true
}

// consoleLog writes a line to the agent log and to the VM console. In debug
// mode stage2 redirects the agent's stdout to guest-agent.log, so a plain
// log.Printf never reaches console.log there; mirrorConsole covers that
// without printing the line twice.
func consoleLog(format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	log.Print(line)
	mirrorConsole(line)
}
