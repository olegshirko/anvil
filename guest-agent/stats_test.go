package main

import "testing"

func TestStatsParsers(t *testing.T) {
	if v, ok := parseProcStatCPU("cpu  100 0 50 850 0 0 0 0 0 0"); !ok || v != 1000*1e9/clockTicks {
		t.Errorf("proc stat = %d %v", v, ok)
	}
	if _, ok := parseProcStatCPU("cpu0 1 2 3"); ok {
		t.Error("per-CPU line taken for the aggregate")
	}
	if m := parseKeyedUint("usage_usec 1500\nuser_usec 1000\n"); m["usage_usec"] != 1500 {
		t.Errorf("cpu.stat = %v", m)
	}
	if parseLimit("max", 42) != 42 || parseLimit("1024", 42) != 1024 {
		t.Error("limit parsing")
	}
	io := parseIOStat("254:0 rbytes=4096 wbytes=8192 rios=1 wios=2 dbytes=0 dios=0\n")
	if len(io) != 2 || io[0]["op"] != "read" || io[0]["value"] != uint64(4096) || io[1]["value"] != uint64(8192) {
		t.Errorf("io.stat = %v", io)
	}
	nets := parseNetDev(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:     100       1    0    0    0     0          0         0      100       1    0    0    0     0       0          0
  eth0:    2048      10    0    1    0     0          0         0     4096      20    0    0    0     0       0          0
`)
	eth, ok := nets["eth0"].(map[string]any)
	if _, hasLo := nets["lo"]; hasLo || !ok || eth["rx_bytes"] != uint64(2048) || eth["tx_bytes"] != uint64(4096) || eth["rx_dropped"] != uint64(1) {
		t.Errorf("net/dev = %v", nets)
	}
}
