package main

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestParseLease(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want netLease
	}{
		{
			name: "full",
			in:   "192.168.64.28 255.255.255.0 192.168.64.1\n",
			want: netLease{IP: net.IPv4(192, 168, 64, 28).To4(), Mask: net.CIDRMask(24, 32), Router: net.IPv4(192, 168, 64, 1).To4()},
		},
		{
			name: "no router",
			in:   "10.0.0.7 255.0.0.0\n",
			want: netLease{IP: net.IPv4(10, 0, 0, 7).To4(), Mask: net.CIDRMask(8, 32)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLease([]byte(tt.in))
			if err != nil {
				t.Fatalf("parseLease(%q): %v", tt.in, err)
			}
			if !got.IP.Equal(tt.want.IP) {
				t.Errorf("IP = %v, want %v", got.IP, tt.want.IP)
			}
			if got.Mask.String() != tt.want.Mask.String() {
				t.Errorf("Mask = %v, want %v", got.Mask, tt.want.Mask)
			}
			if tt.want.Router == nil {
				if got.Router != nil {
					t.Errorf("Router = %v, want nil", got.Router)
				}
			} else if !got.Router.Equal(tt.want.Router) {
				t.Errorf("Router = %v, want %v", got.Router, tt.want.Router)
			}
		})
	}
}

func TestParseLeaseRejectsCorrupt(t *testing.T) {
	for _, in := range []string{
		"",
		"192.168.64.28",
		"192.168.64.28 255.255.255.0 192.168.64.1 extra",
		"not-an-ip 255.255.255.0",
		"192.168.64.28 not-a-mask",
		"192.168.64.28 255.255.255.0 not-a-router",
		"2001:db8::1 255.255.255.0",
	} {
		if _, err := parseLease([]byte(in)); err == nil {
			t.Errorf("parseLease(%q) accepted a corrupt lease", in)
		}
	}
}

func TestARPProbeFrame(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x2a}
	frame, err := arpProbeFrame(mac, net.ParseIP("192.168.64.7"))
	if err != nil {
		t.Fatalf("arpProbeFrame: %v", err)
	}
	want := []byte{
		// Ethernet: broadcast, our MAC, ARP.
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0x02, 0x00, 0x00, 0x00, 0x00, 0x2a,
		0x08, 0x06,
		// ARP: Ethernet, IPv4, 6/4, request.
		0x00, 0x01, 0x08, 0x00, 0x06, 0x04, 0x00, 0x01,
		// Sender hardware, sender protocol 0.0.0.0 (the probe).
		0x02, 0x00, 0x00, 0x00, 0x00, 0x2a,
		0x00, 0x00, 0x00, 0x00,
		// Target hardware zero, target protocol.
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		192, 168, 64, 7,
	}
	if len(frame) != len(want) {
		t.Fatalf("frame length = %d, want %d", len(frame), len(want))
	}
	for i := range want {
		if frame[i] != want[i] {
			t.Fatalf("frame[%d] = %#x, want %#x\nframe: % x", i, frame[i], want[i], frame)
		}
	}
}

func TestARPProbeFrameRejectsBadInput(t *testing.T) {
	if _, err := arpProbeFrame(net.HardwareAddr{1, 2, 3}, net.ParseIP("192.168.64.7")); err == nil {
		t.Error("short source MAC accepted")
	}
	if _, err := arpProbeFrame(net.HardwareAddr{1, 2, 3, 4, 5, 6}, net.ParseIP("2001:db8::1")); err == nil {
		t.Error("IPv6 target accepted")
	}
}

// arpPacket builds a full Ethernet ARP frame for the parser tests.
func arpPacket(dst, sha net.HardwareAddr, op uint16, spa, tpa net.IP) []byte {
	frame := make([]byte, arpFrameLen)
	copy(frame[0:6], dst)
	copy(frame[6:12], sha)
	binary.BigEndian.PutUint16(frame[12:14], arpEtherType)
	binary.BigEndian.PutUint16(frame[14:16], arpHardwareEthernet)
	binary.BigEndian.PutUint16(frame[16:18], arpProtocolIPv4)
	frame[18] = 6
	frame[19] = 4
	binary.BigEndian.PutUint16(frame[20:22], op)
	copy(frame[22:28], sha)
	copy(frame[28:32], spa.To4())
	copy(frame[38:42], tpa.To4())
	return frame
}

func TestARPClaimSource(t *testing.T) {
	ourMAC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x2a}
	otherMAC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x99}
	ourIP := net.ParseIP("192.168.64.28")
	otherIP := net.ParseIP("192.168.64.99")
	broadcast := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	zero := net.IPv4zero.To4()

	t.Run("reply from another host", func(t *testing.T) {
		frame := arpPacket(ourMAC, otherMAC, arpOpReply, ourIP, zero)
		mac, ok := arpClaimSource(frame, ourMAC, ourIP)
		if !ok || mac.String() != otherMAC.String() {
			t.Fatalf("got %v, %v; want %v, true", mac, ok, otherMAC)
		}
	})
	t.Run("gratuitous request from another host", func(t *testing.T) {
		frame := arpPacket(broadcast, otherMAC, arpOpRequest, ourIP, zero)
		if mac, ok := arpClaimSource(frame, ourMAC, ourIP); !ok || mac.String() != otherMAC.String() {
			t.Fatalf("got %v, %v; want %v, true", mac, ok, otherMAC)
		}
	})
	t.Run("our own reply", func(t *testing.T) {
		frame := arpPacket(ourMAC, ourMAC, arpOpReply, ourIP, zero)
		if _, ok := arpClaimSource(frame, ourMAC, ourIP); ok {
			t.Fatal("the guest's own MAC was reported as a duplicate")
		}
	})
	t.Run("plain request asking who has the address", func(t *testing.T) {
		frame := arpPacket(broadcast, otherMAC, arpOpRequest, otherIP, ourIP)
		if _, ok := arpClaimSource(frame, ourMAC, ourIP); ok {
			t.Fatal("an asker was reported as the owner")
		}
	})
	t.Run("claim for a different address", func(t *testing.T) {
		frame := arpPacket(ourMAC, otherMAC, arpOpReply, otherIP, zero)
		if _, ok := arpClaimSource(frame, ourMAC, ourIP); ok {
			t.Fatal("a claim for another address was accepted")
		}
	})
	t.Run("non-ARP frame", func(t *testing.T) {
		frame := arpPacket(ourMAC, otherMAC, arpOpReply, ourIP, zero)
		frame[12], frame[13] = 0x08, 0x00
		if _, ok := arpClaimSource(frame, ourMAC, ourIP); ok {
			t.Fatal("an IPv4 frame was parsed as ARP")
		}
	})
	t.Run("truncated frame", func(t *testing.T) {
		frame := arpPacket(ourMAC, otherMAC, arpOpReply, ourIP, zero)
		if _, ok := arpClaimSource(frame[:20], ourMAC, ourIP); ok {
			t.Fatal("a truncated frame was parsed")
		}
	})
}
