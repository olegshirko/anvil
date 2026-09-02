package main

import (
	"encoding/json"
	"net/http/httptest"
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
		{"ipc unsupported belongs to the 400 layer, not warnings",
			dockerHostConfig{IpcMode: "container:abc"}, nil, []string{"IpcMode"}},
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
		"GroupAdd":["4242"],"IpcMode":"host","UTSMode":"host","Annotations":{"a":"b"},
		"LogConfig":{"Type":"none"}}`
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
	if hc.IpcMode != "host" || hc.UtsMode != "host" || hc.OomScoreAdj != 100 || hc.ShmSize != 67108864 {
		t.Errorf("misc fields: %+v", hc)
	}
	if hc.LogConfig.Type != "none" {
		t.Errorf("LogConfig: %+v", hc.LogConfig)
	}
}

func TestIsEmptyJSON(t *testing.T) {
	for _, raw := range []string{"null", "0", `""`, "[]", "{}", "false", " ", "\n\t0\r\n", ""} {
		if !isEmptyJSON(json.RawMessage(raw)) {
			t.Errorf("%q should be empty", raw)
		}
	}
	for _, raw := range []string{"1", "-1", "true", `"x"`, "[1]", `{"a":1}`, "300"} {
		if isEmptyJSON(json.RawMessage(raw)) {
			t.Errorf("%q should NOT be empty", raw)
		}
	}
}

func TestValidateHostConfig(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // "" means accepted
	}{
		{"no hostconfig", `{"Image":"alpine"}`, ""},
		{"empty hostconfig", `{"Image":"alpine","HostConfig":{}}`, ""},
		{"cli-populated but unset", `{"HostConfig":{"OomKillDisable":false,"BlkioWeight":0,"Isolation":"","Runtime":""}}`, ""},
		{"oom-kill-disable", `{"HostConfig":{"OomKillDisable":true}}`, "OomKillDisable"},
		{"blkio-weight", `{"HostConfig":{"BlkioWeight":500}}`, "BlkioWeight"},
		{"storage-opt", `{"HostConfig":{"StorageOpt":{"size":"1g"}}}`, "StorageOpt"},
		{"isolation", `{"HostConfig":{"Isolation":"hyperv"}}`, "Isolation"},
		{"runtime", `{"HostConfig":{"Runtime":"sysbox"}}`, "Runtime"},
		{"log json-file", `{"HostConfig":{"LogConfig":{"Type":"json-file"}}}`, ""},
		{"log none", `{"HostConfig":{"LogConfig":{"Type":"none"}}}`, ""},
		{"log unset", `{"HostConfig":{"LogConfig":{"Type":""}}}`, ""},
		{"log syslog", `{"HostConfig":{"LogConfig":{"Type":"syslog"}}}`, "syslog"},
		{"null values ignored", `{"HostConfig":{"StorageOpt":null,"Isolation":null}}`, ""},
		{"honored field passes", `{"HostConfig":{"ShmSize":134217728,"PidsLimit":100}}`, ""},
		{"malformed body is not ours", `{"HostConfig": 3`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHostConfig([]byte(tc.body))
			if tc.want == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v does not name %q", err, tc.want)
			}
		})
	}
}

func TestValidateCreatePlatform(t *testing.T) {
	accept := []string{"", "linux", "linux/arm64"}
	reject := []string{"linux/amd64", "linux/amd64/v2", "darwin/arm64", "windows/amd64"}
	for _, p := range accept {
		r := httptest.NewRequest("POST", "/v1.43/containers/create"+platformQuery(p), nil)
		if err := validateCreatePlatform(r); err != nil {
			t.Errorf("platform %q should be accepted, got %v", p, err)
		}
	}
	for _, p := range reject {
		r := httptest.NewRequest("POST", "/v1.43/containers/create"+platformQuery(p), nil)
		err := validateCreatePlatform(r)
		if err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("platform %q should be rejected naming itself, got %v", p, err)
		}
	}
}

func platformQuery(p string) string {
	if p == "" {
		return ""
	}
	return "?platform=" + p
}

func TestMemorySwapSpec(t *testing.T) {
	mem := int64(64 << 20)
	cases := []struct {
		name        string
		memory      int64
		memorySwap  int64
		wantSwap    *int64 // nil = skip
		wantErr     bool
		errFragment string
	}{
		{"unset", mem, 0, nil, false, ""},
		{"unlimited", mem, -1, ptrInt64(-1), false, ""},
		{"twice memory", mem, 128 << 20, ptrInt64(128 << 20), false, ""},
		{"swap equals memory", mem, mem, ptrInt64(mem), false, ""},
		{"requires memory", 0, 128 << 20, nil, true, "requires"},
		{"less than memory", mem, 32 << 20, nil, true, ">="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swap, err := memorySwapSpec(tc.memory, tc.memorySwap)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.errFragment) {
					t.Fatalf("want error containing %q, got %v", tc.errFragment, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantSwap == nil && swap != nil {
				t.Fatalf("want nil swap, got %d", *swap)
			}
			if tc.wantSwap != nil && (swap == nil || *swap != *tc.wantSwap) {
				t.Fatalf("want swap %d, got %v", *tc.wantSwap, swap)
			}
		})
	}
}

func ptrInt64(v int64) *int64 { return &v }

func TestKnownRlimits(t *testing.T) {
	for _, name := range []string{"nofile", "nproc", "core", "stack", "rtprio", "rttime", "msgqueue", "sigpending", "nice", "locks", "memlock", "as", "cpu", "data", "fsize", "rss"} {
		if !knownRlimits["RLIMIT_"+strings.ToUpper(name)] {
			t.Errorf("rlimit %q missing from knownRlimits", name)
		}
	}
	if knownRlimits["RLIMIT_BOGUS"] {
		t.Error("bogus rlimit must not be known")
	}
}
