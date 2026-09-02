package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnsupportedHostConfigWarnings(t *testing.T) {
	yes := true
	no := false
	cases := []struct {
		name string
		hc   dockerHostConfig
		want []string // substrings expected in the warnings
		bad  []string // substrings that must NOT appear
	}{
		{"empty", dockerHostConfig{}, nil, nil},
		{"init true", dockerHostConfig{Init: &yes},
			[]string{"Init"}, nil},
		{"init false", dockerHostConfig{Init: &no}, nil, []string{"Init"}},
		{"userns", dockerHostConfig{UsernsMode: "remap"},
			[]string{"UsernsMode"}, nil},
		{"ipc host ok", dockerHostConfig{IpcMode: "host"}, nil, []string{"IpcMode"}},
		{"ipc shareable ok", dockerHostConfig{IpcMode: "shareable"}, nil, []string{"IpcMode"}},
		{"ipc container", dockerHostConfig{IpcMode: "container:abc"},
			[]string{"IpcMode"}, nil},
		{"nnp ok", dockerHostConfig{SecurityOpt: []string{"no-new-privileges:true"}}, nil, nil},
		{"apparmor ok", dockerHostConfig{SecurityOpt: []string{"apparmor=unconfined"}}, nil, nil},
		{"unknown secopt", dockerHostConfig{SecurityOpt: []string{"something-else=1"}},
			[]string{"SecurityOpt"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unsupportedHostConfigWarnings(tc.hc)
			joined := strings.Join(got, "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("warnings %q missing %q", joined, w)
				}
			}
			for _, b := range tc.bad {
				if strings.Contains(joined, b) {
					t.Errorf("warnings %q unexpectedly contain %q", joined, b)
				}
			}
		})
	}
}

func TestInspectHostConfigLayersSnapshot(t *testing.T) {
	pids := int64(512)
	snap := &dockerHostConfig{
		Memory:     256 << 20,
		PidsLimit:  &pids,
		CpusetCpus: "0-1",
		ShmSize:    128 << 20,
		Ulimits:    []dockerUlimit{{Name: "nofile", Soft: 1024, Hard: 4096}},
	}
	meta := &containerMeta{HostConfig: snap}

	got := inspectHostConfig(meta, dockerHostConfig{
		AutoRemove:   true,
		NetworkMode:  "bridge",
		PortBindings: map[string][]dockerHostPort{},
	})

	if got.Memory != snap.Memory || got.CpusetCpus != snap.CpusetCpus || got.ShmSize != snap.ShmSize {
		t.Errorf("snapshot fields lost: %+v", got)
	}
	if got.PidsLimit == nil || *got.PidsLimit != 512 {
		t.Errorf("PidsLimit not preserved: %+v", got.PidsLimit)
	}
	if len(got.Ulimits) != 1 || got.Ulimits[0].Name != "nofile" {
		t.Errorf("Ulimits not preserved: %+v", got.Ulimits)
	}
	if !got.AutoRemove || got.NetworkMode != "bridge" {
		t.Errorf("live fields not applied: %+v", got)
	}

	// Without a snapshot the live value passes through untouched.
	bare := inspectHostConfig(nil, *snap)
	if bare.Memory != snap.Memory || bare.NetworkMode != "" || bare.AutoRemove {
		t.Errorf("nil meta must pass the live HostConfig through: %+v", bare)
	}
}

func TestHostConfigJSONRoundTrip(t *testing.T) {
	// The struct doubles as the inspect output; the docker field names must
	// survive a marshal/unmarshal round trip.
	in := `{"CpuShares":1024,"CpuPeriod":100000,"CpuQuota":50000,"CpusetCpus":"0-1",
		"MemorySwap":536870912,"PidsLimit":256,"ShmSize":67108864,"OomScoreAdj":100,
		"SecurityOpt":["no-new-privileges"],"Ulimits":[{"Name":"nofile","Soft":1024,"Hard":4096}],
		"GroupAdd":["wheel"],"IpcMode":"host","Annotations":{"a":"b"}}`
	var hc dockerHostConfig
	if err := json.Unmarshal([]byte(in), &hc); err != nil {
		t.Fatal(err)
	}
	if hc.CpuShares != 1024 || hc.CpuQuota != 50000 || hc.CpusetCpus != "0-1" {
		t.Errorf("cpu fields: %+v", hc)
	}
	if hc.PidsLimit == nil || *hc.PidsLimit != 256 {
		t.Errorf("PidsLimit: %+v", hc.PidsLimit)
	}
	if len(hc.Ulimits) != 1 || hc.Ulimits[0].Hard != 4096 {
		t.Errorf("Ulimits: %+v", hc.Ulimits)
	}
	if hc.IpcMode != "host" || hc.OomScoreAdj != 100 || hc.ShmSize != 67108864 {
		t.Errorf("misc fields: %+v", hc)
	}
}
