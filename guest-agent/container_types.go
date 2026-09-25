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
	Image            string                `json:"Image"`
	Volumes          map[string]struct{}   `json:"Volumes"` // anonymous `-v /path`
	Labels           map[string]string     `json:"Labels"`
	NetworkingConfig *dockerNetworkingConf `json:"NetworkingConfig,omitempty"`
	HostConfig       dockerHostConfig      `json:"HostConfig"`
	Healthcheck      *dockerHealthcheck    `json:"Healthcheck,omitempty"`
}

type dockerNetworkingConf struct {
	EndpointsConfig map[string]dockerEndpoint `json:"EndpointsConfig"`
}

type dockerEndpoint struct {
	Aliases []string `json:"Aliases"`
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
}

type dockerVolumeOptions struct {
	NoCopy bool `json:"NoCopy"`
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
}

// dockerContainerInspect is a minimal subset of GET /containers/{id}/json.
type dockerContainerInspect struct {
	Id              string                `json:"Id"`
	Name            string                `json:"Name"`
	Image           string                `json:"Image"`
	State           dockerContainerState  `json:"State"`
	Config          dockerContainerConfig `json:"Config"`
	HostConfig      dockerHostConfig      `json:"HostConfig"`
	NetworkSettings dockerNetworkSettings `json:"NetworkSettings"`
	Mounts          []dockerMountPoint    `json:"Mounts"`
	// Docker exposes RestartCount at the top level (not under State).
	RestartCount int `json:"RestartCount"`
}

type dockerContainerState struct {
	Status   string             `json:"Status"`
	Running  bool               `json:"Running"`
	Pid      int                `json:"Pid"`
	ExitCode int                `json:"ExitCode"`
	Health   *dockerHealthState `json:"Health,omitempty"`
}

type dockerContainerConfig struct {
	Labels      map[string]string  `json:"Labels"`
	Image       string             `json:"Image"`
	Healthcheck *dockerHealthcheck `json:"Healthcheck,omitempty"`
	Tty         bool               `json:"Tty,omitempty"`
	OpenStdin   bool               `json:"OpenStdin,omitempty"`
	Env         []string           `json:"Env,omitempty"`
	Cmd         []string           `json:"Cmd,omitempty"`
	Entrypoint  []string           `json:"Entrypoint,omitempty"`
	WorkingDir  string             `json:"WorkingDir,omitempty"`
	StopSignal  string             `json:"StopSignal,omitempty"`
}

type dockerNetworkSettings struct {
	IPAddress string                         `json:"IPAddress"`
	Ports     map[string][]dockerHostPort    `json:"Ports,omitempty"`
	Networks  map[string]dockerEndpointStats `json:"Networks,omitempty"`
}

type dockerEndpointStats struct {
	IPAddress   string `json:"IPAddress"`
	IPPrefixLen int    `json:"IPPrefixLen"`
	MacAddress  string `json:"MacAddress,omitempty"`
}

// dockerPort matches the Docker API port binding shape.
type dockerPort struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}
