package limaconfig

import (
	"net"

	"anvil/internal/environment"
)

// Arch is the CPU architecture of the VM guest.
type Arch = environment.Arch

// VMType identifies the hypervisor backend.
type VMType = string

// MountType identifies the filesystem sharing protocol.
type MountType = string

// Proto is a network protocol name.
type Proto = string

// ProvisionMode controls when a provisioning script runs.
type ProvisionMode = string

const (
	QEMU    VMType = "qemu"
	VZ      VMType = "vz"
	Krunkit VMType = "krunkit"

	REVSSHFS MountType = "reverse-sshfs"
	VIRTIOFS MountType = "virtiofs"
	NINEP    MountType = "9p"

	TCP Proto = "tcp"
	UDP Proto = "udp"

	ProvisionModeSystem     ProvisionMode = "system"
	ProvisionModeUser       ProvisionMode = "user"
	ProvisionModeBoot       ProvisionMode = "boot"
	ProvisionModeDependency ProvisionMode = "dependency"
	ProvisionModeData       ProvisionMode = "data"
)

// Config is the root configuration object passed to limactl.
type Config struct {
	VMType               VMType            `yaml:"vmType,omitempty" json:"vmType,omitempty"`
	Arch                 Arch              `yaml:"arch,omitempty"`
	CPUs                 *int              `yaml:"cpus,omitempty"`
	Memory               string            `yaml:"memory,omitempty"`
	Disk                 string            `yaml:"disk,omitempty"`
	VMOpts               VMOpts            `yaml:"vmOpts,omitempty" json:"vmOpts,omitempty"`
	Images               []File            `yaml:"images"`
	AdditionalDisks      []Disk            `yaml:"additionalDisks,omitempty" json:"additionalDisks,omitempty"`
	Mounts               []Mount           `yaml:"mounts,omitempty"`
	MountType            MountType         `yaml:"mountType,omitempty" json:"mountType,omitempty"`
	SSH                  SSH               `yaml:"ssh"`
	Containerd           Containerd        `yaml:"containerd"`
	DNS                  []net.IP          `yaml:"dns"`
	Env                  map[string]string `yaml:"env,omitempty"`
	Firmware             Firmware          `yaml:"firmware"`
	HostResolver         HostResolver      `yaml:"hostResolver"`
	PortForwards         []PortForward     `yaml:"portForwards,omitempty"`
	Networks             []Network         `yaml:"networks,omitempty"`
	Provision            []Provision       `yaml:"provision,omitempty" json:"provision,omitempty"`
	NestedVirtualization bool              `yaml:"nestedVirtualization,omitempty" json:"nestedVirtualization,omitempty"`
}

// File describes a VM disk image or kernel/initrd pair.
type File struct {
	Location string      `yaml:"location"` // REQUIRED
	Arch     Arch        `yaml:"arch,omitempty"`
	Digest   string      `yaml:"digest,omitempty"`
	Kernel   *FileKernel `yaml:"kernel,omitempty" json:"kernel,omitempty"`
	Initrd   *FileInitrd `yaml:"initrd,omitempty" json:"initrd,omitempty"`
}

// FileKernel points to a kernel binary.
type FileKernel struct {
	Location string `yaml:"location"` // REQUIRED
	Digest   string `yaml:"digest,omitempty"`
	Cmdline  string `yaml:"cmdline,omitempty"`
}

// FileInitrd points to an initrd image.
type FileInitrd struct {
	Location string `yaml:"location"` // REQUIRED
	Digest   string `yaml:"digest,omitempty"`
}

// Mount maps a host directory into the guest.
type Mount struct {
	Location   string `yaml:"location"` // REQUIRED
	MountPoint string `yaml:"mountPoint,omitempty"`
	Writable   bool   `yaml:"writable"`
	NineP      NineP  `yaml:"9p,omitempty" json:"9p,omitempty"`
}

// NineP tunes the 9P filesystem protocol.
type NineP struct {
	SecurityModel   string `yaml:"securityModel,omitempty" json:"securityModel,omitempty"`
	ProtocolVersion string `yaml:"protocolVersion,omitempty" json:"protocolVersion,omitempty"`
	Msize           string `yaml:"msize,omitempty" json:"msize,omitempty"`
	Cache           string `yaml:"cache,omitempty" json:"cache,omitempty"`
}

// Disk describes an extra virtual disk attached to the VM.
type Disk struct {
	Name   string   `yaml:"name" json:"name"` // REQUIRED
	Format bool     `yaml:"format" json:"format"`
	FSType string   `yaml:"fsType,omitempty" json:"fsType,omitempty"`
	FSArgs []string `yaml:"fsArgs,omitempty" json:"fsArgs,omitempty"`
}

// SSH exposes the guest SSH daemon to the host.
type SSH struct {
	LocalPort         int  `yaml:"localPort,omitempty"`
	LoadDotSSHPubKeys bool `yaml:"loadDotSSHPubKeys"`
	ForwardAgent      bool `yaml:"forwardAgent"` // default: false
}

// Containerd toggles built-in containerd provisioning.
type Containerd struct {
	System bool `yaml:"system"` // default: false
	User   bool `yaml:"user"`   // default: true
}

// Firmware controls the VM firmware settings.
type Firmware struct {
	// LegacyBIOS disables UEFI. Ignored for aarch64.
	LegacyBIOS bool `yaml:"legacyBIOS"`
}

// HostResolver configures DNS resolution inside the VM.
type HostResolver struct {
	Enabled bool              `yaml:"enabled" json:"enabled"`
	IPv6    bool              `yaml:"ipv6,omitempty" json:"ipv6,omitempty"`
	Hosts   map[string]string `yaml:"hosts,omitempty" json:"hosts,omitempty"`
}

// PortForward maps a host port/socket to a guest port/socket.
type PortForward struct {
	GuestIPMustBeZero bool   `yaml:"guestIPMustBeZero,omitempty" json:"guestIPMustBeZero,omitempty"`
	GuestIP           net.IP `yaml:"guestIP,omitempty" json:"guestIP,omitempty"`
	GuestPort         int    `yaml:"guestPort,omitempty" json:"guestPort,omitempty"`
	GuestPortRange    [2]int `yaml:"guestPortRange,omitempty" json:"guestPortRange,omitempty"`
	GuestSocket       string `yaml:"guestSocket,omitempty" json:"guestSocket,omitempty"`
	HostIP            net.IP `yaml:"hostIP,omitempty" json:"hostIP,omitempty"`
	HostPort          int    `yaml:"hostPort,omitempty" json:"hostPort,omitempty"`
	HostPortRange     [2]int `yaml:"hostPortRange,omitempty" json:"hostPortRange,omitempty"`
	HostSocket        string `yaml:"hostSocket,omitempty" json:"hostSocket,omitempty"`
	Proto             Proto  `yaml:"proto,omitempty" json:"proto,omitempty"`
	Ignore            bool   `yaml:"ignore,omitempty" json:"ignore,omitempty"`
}

// Network configures a guest network interface.
type Network struct {
	// Lima, Socket and VNL are mutually exclusive; exactly one must be set.
	Lima   string `yaml:"lima,omitempty" json:"lima,omitempty"`
	Socket string `yaml:"socket,omitempty" json:"socket,omitempty"`
	VZNAT  bool   `yaml:"vzNAT,omitempty" json:"vzNAT,omitempty"`

	VNLDeprecated        string `yaml:"vnl,omitempty" json:"vnl,omitempty"`
	SwitchPortDeprecated uint16 `yaml:"switchPort,omitempty" json:"switchPort,omitempty"`
	MACAddress           string `yaml:"macAddress,omitempty" json:"macAddress,omitempty"`
	Interface            string `yaml:"interface,omitempty" json:"interface,omitempty"`
	Metric               uint32 `yaml:"metric,omitempty" json:"metric,omitempty"`
}

// Provision describes a script to run inside the VM at a specific lifecycle stage.
type Provision struct {
	Mode           ProvisionMode `yaml:"mode" json:"mode"` // default: "system"
	Script         string        `yaml:"script,omitempty" json:"script,omitempty"`
	Path           string        `yaml:"path,omitempty" json:"path,omitempty"`
	Content        string        `yaml:"content,omitempty" json:"content,omitempty"`
	SkipResolution bool          `yaml:"skipDefaultDependencyResolution,omitempty" json:"skipDefaultDependencyResolution,omitempty"`
}

// VMOpts holds hypervisor-specific options.
type VMOpts struct {
	QEMU   QEMUOpts `yaml:"qemu,omitempty" json:"qemu,omitempty"`
	VZOpts VZOpts   `yaml:"vz,omitempty" json:"vz,omitempty"`
}

// QEMUOpts fine-tunes the QEMU backend.
type QEMUOpts struct {
	MinimumVersion *string         `yaml:"minimumVersion,omitempty" json:"minimumVersion,omitempty"`
	CPUType        map[Arch]string `yaml:"cpuType,omitempty" json:"cpuType,omitempty"`
	ExtraArgs      []string        `yaml:"extraArgs,omitempty" json:"extraArgs,omitempty"`
}

// VZOpts fine-tunes the Apple Virtualization backend.
type VZOpts struct {
	Rosetta Rosetta `yaml:"rosetta,omitempty" json:"rosetta,omitempty"`
}

// Rosetta enables x86_64 emulation on Apple Silicon.
type Rosetta struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	BinFmt  bool `yaml:"binfmt" json:"binfmt"`
}
