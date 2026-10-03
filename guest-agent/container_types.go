package main

// Docker Engine API shapes for containers: the create request (with
// HostConfig) and the list/inspect responses.

// dockerCreateRequest mirrors the minimal parts of Docker's container creation body.
type dockerCreateRequest struct {
	Hostname         string                `json:"Hostname"`
	Domainname       string                `json:"Domainname"`
	User             string                `json:"User"`
	AttachStdin      bool                  `json:"AttachStdin"`
	AttachStdout     bool                  `json:"AttachStdout"`
	AttachStderr     bool                  `json:"AttachStderr"`
	Tty              bool                  `json:"Tty"`
	OpenStdin        bool                  `json:"OpenStdin"`
	StdinOnce        bool                  `json:"StdinOnce"`
	Env              []string              `json:"Env"`
	Cmd              []string              `json:"Cmd"`
	Entrypoint       []string              `json:"Entrypoint"`
	WorkingDir       string                `json:"WorkingDir"`
	StopSignal       string                `json:"StopSignal"`
	StopTimeout      *int                  `json:"StopTimeout,omitempty"`
	Image            string                `json:"Image"`
	Volumes          map[string]struct{}   `json:"Volumes"` // anonymous `-v /path`
	ExposedPorts     map[string]struct{}   `json:"ExposedPorts"`
	Labels           map[string]string     `json:"Labels"`
	NetworkingConfig *dockerNetworkingConf `json:"NetworkingConfig,omitempty"`
	HostConfig       dockerHostConfig      `json:"HostConfig"`
	Healthcheck      *dockerHealthcheck    `json:"Healthcheck,omitempty"`

	// generatedName marks a name the agent picked (no --name): the
	// hostname then stays the short ID, as with Docker.
	generatedName bool
}

type dockerNetworkingConf struct {
	EndpointsConfig map[string]dockerEndpoint `json:"EndpointsConfig"`
}

type dockerEndpoint struct {
	Aliases    []string            `json:"Aliases"`
	IPAMConfig *dockerEndpointIPAM `json:"IPAMConfig,omitempty"`
}

// dockerEndpointIPAM is a requested static address (`--ip`, compose
// ipv4_address).
type dockerEndpointIPAM struct {
	IPv4Address string `json:"IPv4Address"`
}

type dockerHostConfig struct {
	Binds           []string                    `json:"Binds"`
	Mounts          []dockerMount               `json:"Mounts"`
	NetworkMode     string                      `json:"NetworkMode"`
	PortBindings    map[string][]dockerHostPort `json:"PortBindings"`
	RestartPolicy   dockerRestartPolicy         `json:"RestartPolicy"`
	AutoRemove      bool                        `json:"AutoRemove"`
	Privileged      bool                        `json:"Privileged"`
	PublishAllPorts bool                        `json:"PublishAllPorts"`
	ExtraHosts      []string                    `json:"ExtraHosts"`
	Memory          int64                       `json:"Memory"`
	NanoCpus        int64                       `json:"NanoCpus"`
	CapAdd          []string                    `json:"CapAdd"`
	CapDrop         []string                    `json:"CapDrop"`
	ReadonlyRootfs  bool                        `json:"ReadonlyRootfs"`
	PidMode         string                      `json:"PidMode"`
	TmpFs           map[string]string           `json:"Tmpfs"`
	Dns             []string                    `json:"Dns"`
	DnsSearch       []string                    `json:"DnsSearch"`
	DnsOptions      []string                    `json:"DnsOptions"`
	Sysctls         map[string]string           `json:"Sysctls"`
	Devices         []dockerDevice              `json:"Devices"`
	Links           []string                    `json:"Links"`
	VolumesFrom     []string                    `json:"VolumesFrom"`

	// Resource knobs beyond Memory/NanoCpus. Zero values mean "unset" and are
	// skipped; docker semantics for sentinel values (-1) are honored.
	CpuShares         int64             `json:"CpuShares"`
	CpuPeriod         int64             `json:"CpuPeriod"`
	CpuQuota          int64             `json:"CpuQuota"`
	CpusetCpus        string            `json:"CpusetCpus"`
	CpusetMems        string            `json:"CpusetMems"`
	MemorySwap        int64             `json:"MemorySwap"`
	MemoryReservation int64             `json:"MemoryReservation"`
	PidsLimit         *int64            `json:"PidsLimit"`
	ShmSize           int64             `json:"ShmSize"`
	OomScoreAdj       int               `json:"OomScoreAdj"`
	SecurityOpt       []string          `json:"SecurityOpt"`
	Ulimits           []dockerUlimit    `json:"Ulimits"`
	GroupAdd          []string          `json:"GroupAdd"`
	IpcMode           string            `json:"IpcMode"`
	CgroupnsMode      string            `json:"CgroupnsMode"`
	UsernsMode        string            `json:"UsernsMode"`
	Init              *bool             `json:"Init"`
	Annotations       map[string]string `json:"Annotations"`
	UtsMode           string            `json:"UTSMode"`
	LogConfig         dockerLogConfig   `json:"LogConfig"`
}

type dockerLogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

type dockerUlimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

type dockerHostPort struct {
	HostIp   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerMount struct {
	Type          string               `json:"Type"`
	Source        string               `json:"Source"`
	Target        string               `json:"Target"`
	ReadOnly      bool                 `json:"ReadOnly"`
	VolumeOptions *dockerVolumeOptions `json:"VolumeOptions,omitempty"`
	TmpfsOptions  *dockerTmpfsOptions  `json:"TmpfsOptions,omitempty"`
}

// dockerTmpfsOptions is --mount type=tmpfs,tmpfs-size=…,tmpfs-mode=….
type dockerTmpfsOptions struct {
	SizeBytes int64      `json:"SizeBytes,omitempty"`
	Mode      uint32     `json:"Mode,omitempty"`
	Options   [][]string `json:"Options,omitempty"` // [["exec"]] lifts noexec
}

type dockerVolumeOptions struct {
	NoCopy  bool   `json:"NoCopy"`
	Subpath string `json:"Subpath,omitempty"` // mount only this path of the volume (API 1.45)
}

type dockerDevice struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"`
}

type dockerRestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type dockerHealthcheck struct {
	Test        []string `json:"Test"`
	Interval    int64    `json:"Interval"`
	Timeout     int64    `json:"Timeout"`
	Retries     int      `json:"Retries"`
	StartPeriod int64    `json:"StartPeriod,omitempty"`
	// StartInterval is the probe interval during StartPeriod (API 1.44).
	StartInterval int64 `json:"StartInterval,omitempty"`
}

type dockerCreateResponse struct {
	Id       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

// dockerHostConfig (the create request type above) doubles as the
// inspect response shape: the docker CLI and compose dereference
// HostConfig.AutoRemove / PortBindings on inspect (e.g.
// cli/command/container/start.go — a missing object nil-panics the CLI).
// dockerRestartPolicy likewise already exists with the create request.

// dockerContainerSummary matches the JSON returned by GET /containers/json.
type dockerContainerSummary struct {
	Id      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	Ports   []dockerPort      `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	// Mounts: compose's recreate reads the old container's anonymous
	// volumes from the list endpoint, not from inspect.
	Mounts []dockerMountPoint `json:"Mounts"`
	// HostConfig/NetworkSettings: compose reads the network mode and the
	// endpoints from the list too.
	HostConfig      dockerSummaryHostConfig      `json:"HostConfig"`
	NetworkSettings dockerSummaryNetworkSettings `json:"NetworkSettings"`
	// Sizes, only with ?size=1 (docker ps -s).
	SizeRw     *int64 `json:"SizeRw,omitempty"`
	SizeRootFs *int64 `json:"SizeRootFs,omitempty"`

	// Filter inputs that are not part of the list payload.
	exitCode   int
	created    int64 // UnixNano: before/since order containers made in the same second
	networks   []string
	networkIDs map[string]string
	health     string
	exposed    []string
}

// dockerContainerInspect is a minimal subset of GET /containers/{id}/json.
type dockerContainerInspect struct {
	SizeRw          *int64                `json:"SizeRw,omitempty"`
	SizeRootFs      *int64                `json:"SizeRootFs,omitempty"`
	Id              string                `json:"Id"`
	Created         string                `json:"Created"`
	Path            string                `json:"Path"`
	Args            []string              `json:"Args"`
	Name            string                `json:"Name"`
	Image           string                `json:"Image"` // the image ID; Config.Image is the name
	ResolvConfPath  string                `json:"ResolvConfPath"`
	HostnamePath    string                `json:"HostnamePath"`
	HostsPath       string                `json:"HostsPath"`
	LogPath         string                `json:"LogPath"`
	Driver          string                `json:"Driver"`
	Platform        string                `json:"Platform"`
	State           dockerContainerState  `json:"State"`
	Config          dockerContainerConfig `json:"Config"`
	HostConfig      dockerHostConfig      `json:"HostConfig"`
	NetworkSettings dockerNetworkSettings `json:"NetworkSettings"`
	Mounts          []dockerMountPoint    `json:"Mounts"`
	// Docker exposes RestartCount at the top level (not under State).
	RestartCount int `json:"RestartCount"`
}

type dockerContainerState struct {
	Status     string             `json:"Status"`
	Running    bool               `json:"Running"`
	Paused     bool               `json:"Paused"`
	Restarting bool               `json:"Restarting"`
	OOMKilled  bool               `json:"OOMKilled"`
	Dead       bool               `json:"Dead"`
	Pid        int                `json:"Pid"`
	ExitCode   int                `json:"ExitCode"`
	Error      string             `json:"Error"`
	StartedAt  string             `json:"StartedAt"`
	FinishedAt string             `json:"FinishedAt"`
	Health     *dockerHealthState `json:"Health,omitempty"`
}

type dockerContainerConfig struct {
	Hostname     string              `json:"Hostname"`
	Domainname   string              `json:"Domainname"`
	User         string              `json:"User"`
	AttachStdin  bool                `json:"AttachStdin"`
	AttachStdout bool                `json:"AttachStdout"`
	AttachStderr bool                `json:"AttachStderr"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	StdinOnce    bool                `json:"StdinOnce"`
	Labels       map[string]string   `json:"Labels"`
	Image        string              `json:"Image"`
	Healthcheck  *dockerHealthcheck  `json:"Healthcheck,omitempty"`
	Tty          bool                `json:"Tty,omitempty"`
	OpenStdin    bool                `json:"OpenStdin,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
}

type dockerNetworkSettings struct {
	IPAddress string `json:"IPAddress"`
	// The primary network's IPv6 address (the legacy top-level fields,
	// as Docker fills them).
	GlobalIPv6Address   string                         `json:"GlobalIPv6Address,omitempty"`
	GlobalIPv6PrefixLen int                            `json:"GlobalIPv6PrefixLen,omitempty"`
	IPv6Gateway         string                         `json:"IPv6Gateway,omitempty"`
	Ports               map[string][]dockerHostPort    `json:"Ports,omitempty"`
	Networks            map[string]dockerEndpointStats `json:"Networks,omitempty"`
}

type dockerEndpointStats struct {
	NetworkID           string   `json:"NetworkID,omitempty"`
	Gateway             string   `json:"Gateway,omitempty"`
	IPAddress           string   `json:"IPAddress"`
	IPPrefixLen         int      `json:"IPPrefixLen"`
	IPv6Gateway         string   `json:"IPv6Gateway,omitempty"`
	GlobalIPv6Address   string   `json:"GlobalIPv6Address,omitempty"`
	GlobalIPv6PrefixLen int      `json:"GlobalIPv6PrefixLen,omitempty"`
	MacAddress          string   `json:"MacAddress,omitempty"`
	Aliases             []string `json:"Aliases,omitempty"`
	DNSNames            []string `json:"DNSNames,omitempty"`
}

// dockerPort matches the Docker API port binding shape.
type dockerPort struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

type dockerSummaryHostConfig struct {
	NetworkMode string `json:"NetworkMode"`
}

type dockerSummaryNetworkSettings struct {
	Networks map[string]dockerEndpointStats `json:"Networks"`
}
