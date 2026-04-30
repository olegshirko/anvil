package lima

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"

	"anvil/internal/domain"
	"anvil/internal/environment"
	"anvil/internal/environment/container/containerd"
	"anvil/internal/environment/container/docker"
	"anvil/internal/environment/container/incus"
	"anvil/internal/environment/vm/lima/limaconfig"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/service"
	"anvil/internal/service/process/vmnet"
	"anvil/internal/util"
)

var (
	limaCacheKey   string
	limaCacheVal   limaconfig.Config
	limaCacheErr   error
	limaCacheMutex sync.Mutex
)

func computeCacheKey(conf domain.Config, profileID, cacheDir, profileConfigDir string) string {
	b, _ := json.Marshal(conf)
	h := sha256.New()
	h.Write(b)
	h.Write([]byte(profileID))
	h.Write([]byte(cacheDir))
	h.Write([]byte(profileConfigDir))
	return hex.EncodeToString(h.Sum(nil))
}

// buildLimaConfig translates the Anvil domain config into a Lima YAML configuration.
func buildLimaConfig(ctx context.Context, conf domain.Config, profile *domain.Profile, cacheDir string, profileConfigDir string) (out limaconfig.Config, err error) {
	key := computeCacheKey(conf, profile.ID, cacheDir, profileConfigDir)
	limaCacheMutex.Lock()
	if key == limaCacheKey {
		out = limaCacheVal
		err = limaCacheErr
		limaCacheMutex.Unlock()
		return
	}
	limaCacheMutex.Unlock()

	defer func() {
		limaCacheMutex.Lock()
		limaCacheKey = key
		limaCacheVal = out
		limaCacheErr = err
		limaCacheMutex.Unlock()
	}()

	out.Arch = environment.Arch(conf.Arch).Value()
	hostArchMatch := environment.HostArch() == out.Arch

	// Default to QEMU unless VZ or krunkit conditions are met.
	out.VMType = limaconfig.QEMU

	// VZ on macOS 13+ with matching architecture.
	if util.AtLeastVentura() && conf.VMType == limaconfig.VZ && hostArchMatch {
		out.VMType = limaconfig.VZ

		if conf.VZRosetta && util.AppleSiliconAndModernOS() {
			if util.IsRosettaActive() {
				out.VMOpts.VZOpts.Rosetta.Enabled = true
				out.VMOpts.VZOpts.Rosetta.BinFmt = true
			} else {
				logrus.Warnln("Rosetta2 is not installed; run 'softwareupdate --install-rosetta' to enable it")
			}
		}

		if util.SupportsNestedVirt() {
			out.NestedVirtualization = conf.NestedVirtualization
		}
	}

	// krunkit on Apple Silicon with matching architecture.
	if util.AppleSiliconAndModernOS() && conf.VMType == limaconfig.Krunkit && hostArchMatch {
		out.VMType = limaconfig.Krunkit
	}

	if conf.CPUType != "" && conf.CPUType != "host" {
		out.VMOpts.QEMU.CPUType = map[environment.Arch]string{
			out.Arch: conf.CPUType,
		}
	}

	if out.VMType == limaconfig.QEMU {
		out.VMOpts.QEMU.ExtraArgs = append(out.VMOpts.QEMU.ExtraArgs,
			"-object", "rng-random,id=rng0,filename=/dev/urandom",
			"-device", "virtio-rng-pci,rng=rng0",
		)
	}

	if conf.CPU > 0 {
		out.CPUs = &conf.CPU
	}
	if conf.Memory > 0 {
		out.Memory = fmt.Sprintf("%dMiB", uint32(conf.Memory*1024))
	}
	if conf.RootDisk > 0 {
		out.Disk = fmt.Sprintf("%dGiB", conf.RootDisk)
	}

	out.SSH = limaconfig.SSH{LocalPort: conf.SSHPort, LoadDotSSHPubKeys: false, ForwardAgent: conf.ForwardAgent}
	out.Containerd = limaconfig.Containerd{System: false, User: false}

	out.DNS = conf.Network.DNSResolvers
	if len(out.DNS) == 0 {
		if hostDNS := util.HostDNSResolvers(); len(hostDNS) > 0 {
			out.DNS = hostDNS
		}
	}
	out.HostResolver.Enabled = len(out.DNS) == 0
	out.HostResolver.Hosts = conf.Network.DNSHosts
	if out.HostResolver.Hosts == nil {
		out.HostResolver.Hosts = make(map[string]string)
	}
	if _, ok := out.HostResolver.Hosts["host.docker.internal"]; !ok {
		out.HostResolver.Hosts["host.docker.internal"] = "host.lima.internal"
	}

	out.Env = conf.Env
	if out.Env == nil {
		out.Env = make(map[string]string)
	}

	hostname := profile.ID
	if conf.Hostname != "" {
		hostname = conf.Hostname
	}

	// --- provisioning scripts ---

	// Pre-generate SSH host keys so cloud-init doesn't delay every boot.
	out.Provision = append(out.Provision, limaconfig.Provision{
		Mode: limaconfig.ProvisionModeBoot,
		Script: `for kt in ed25519 ecdsa rsa; do
    file="/etc/ssh/ssh_host_${kt}_key"
    if [ "$kt" = "rsa" ]; then file="/etc/ssh/ssh_host_rsa_key"; fi
    if [ ! -f "$file" ]; then
        case "$kt" in
            ed25519) ssh-keygen -t ed25519 -f /etc/ssh/ssh_host_ed25519_key -N "" >/dev/null 2>&1 ;;
            ecdsa)   ssh-keygen -t ecdsa   -f /etc/ssh/ssh_host_ecdsa_key   -N "" >/dev/null 2>&1 ;;
            rsa)     ssh-keygen -t rsa -b 2048 -f /etc/ssh/ssh_host_rsa_key -N "" >/dev/null 2>&1 ;;
        esac
    fi
done`,
	})

	limaHostname := "lima-" + hostname

	// Ensure /etc/hosts contains the hostname early.
	// Lima sets the guest hostname to "lima-<profile>" via cloud-init meta-data,
	// so we need both entries to prevent sudo DNS resolution delays during boot.
	out.Provision = append(out.Provision, limaconfig.Provision{
		Mode: limaconfig.ProvisionModeBoot,
		Script: `for h in "` + hostname + `" "` + limaHostname + `"; do
	grep -q "127.0.0.1 $h" /etc/hosts || echo "127.0.0.1 $h" >> /etc/hosts
done`,
	})

	// Prevent cloud-init from regenerating SSH keys.
	out.Provision = append(out.Provision, limaconfig.Provision{
		Mode:    limaconfig.ProvisionModeData,
		Path:    "/etc/cloud/cloud.cfg.d/99-anvil.cfg",
		Content: "ssh_deletekeys: false\nssh_genkeytypes: []\n",
	})

	// Restrict cloud-init datasources to avoid the EC2 datasource timeout on
	// every boot. Lima always uses NoCloud, so scanning other datasources is
	// unnecessary and adds 2–30 seconds to startup depending on the image.
	out.Provision = append(out.Provision, limaconfig.Provision{
		Mode:    limaconfig.ProvisionModeData,
		Path:    "/etc/cloud/cloud.cfg.d/98-anvil-datasource.cfg",
		Content: "datasource_list: [NoCloud, None]\ndatasource:\n  NoCloud:\n    fs_label: cidata\n",
	})

	// Speed up systemd-networkd-wait-online by only requiring one interface
	// instead of waiting for all of them. Saves ~1 second on every boot.
	out.Provision = append(out.Provision, limaconfig.Provision{
		Mode:    limaconfig.ProvisionModeData,
		Path:    "/etc/systemd/system/systemd-networkd-wait-online.service.d/override.conf",
		Content: "[Service]\nExecStart=\nExecStart=/usr/lib/systemd/systemd-networkd-wait-online --any\n",
	})

	// Boost inotify limits.
	out.Provision = append(out.Provision, limaconfig.Provision{
		Mode:   limaconfig.ProvisionModeSystem,
		Script: "sysctl -w fs.inotify.max_user_watches=1048576",
	})

	// Disable docker.service auto-start in the base image. The Ubuntu cloud
	// images with Docker pre-installed enable the unit by default, which causes
	// systemd to start Docker during boot (~1 s). Anvil manages the Docker
	// lifecycle explicitly via systemctl start/restart, so the auto-start is
	// redundant and delays VM availability.
	if conf.Runtime == docker.Name {
		out.Provision = append(out.Provision, limaconfig.Provision{
			Mode:   limaconfig.ProvisionModeSystem,
			Script: "systemctl disable docker.service 2>/dev/null || true",
		})
	}

	// Disable docker.service auto-start in the base image. The Ubuntu cloud
	// images with Docker pre-installed enable the unit by default, which causes
	// systemd to start Docker during boot (~1 s). Anvil manages the Docker
	// lifecycle explicitly via systemctl start/restart, so the auto-start is
	// redundant and delays VM availability.
	if conf.Runtime == docker.Name {
		out.Provision = append(out.Provision, limaconfig.Provision{
			Mode:   limaconfig.ProvisionModeSystem,
			Script: "systemctl disable docker.service 2>/dev/null || true",
		})
	}

	// Runtime-specific group membership.
	if conf.Runtime == docker.Name {
		out.Provision = append(out.Provision, limaconfig.Provision{
			Mode:   limaconfig.ProvisionModeDependency,
			Script: "groupadd -f docker && usermod -aG docker {{ .User }}",
		})
	}
	if conf.Runtime == incus.Name {
		out.Provision = append(out.Provision, limaconfig.Provision{
			Mode:   limaconfig.ProvisionModeDependency,
			Script: "groupadd -f incus-admin && usermod -aG incus-admin {{ .User }}",
		})
	}

	// Set hostname inside the VM.
	out.Provision = append(out.Provision, limaconfig.Provision{
		Mode: limaconfig.ProvisionModeSystem,
		Script: `for h in "` + hostname + `" "` + limaHostname + `"; do
	grep -q "127.0.0.1 $h" /etc/hosts || echo "127.0.0.1 $h" >> /etc/hosts
done`,
	})
	out.Provision = append(out.Provision, limaconfig.Provision{
		Mode:   limaconfig.ProvisionModeSystem,
		Script: "hostnamectl set-hostname " + hostname,
	})

	// --- networking ---

	out.Networks = append(out.Networks, limaconfig.Network{Lima: "user-v2"})

	reachableIP := true
	if conf.Network.Address {
		metric := limautil.DefaultRouteMetric
		if conf.Network.PreferredRoute {
			metric = limautil.PreferredRouteMetric
		}

		if out.VMType == limaconfig.VZ && conf.Runtime != incus.Name && conf.Network.Mode != "bridged" {
			out.Networks = append(out.Networks, limaconfig.Network{
				VZNAT:     true,
				Interface: limautil.DefaultInterfaceName,
				Metric:    metric,
			})
		} else {
			reachableIP, _ = ctx.Value(service.ContextKey(vmnet.Name)).(bool)
			if util.IsMacOS() && reachableIP {
				if sktErr := func() error {
					socketFile := vmnet.ServicePaths(profile.ShortName).Socket.Path()
					if _, stErr := os.Stat(socketFile); stErr != nil {
						return fmt.Errorf("vmnet socket missing: %w", stErr)
					}
					out.Networks = append(out.Networks, limaconfig.Network{
						Socket:    socketFile,
						Interface: limautil.DefaultInterfaceName,
						Metric:    metric,
					})
					return nil
				}(); sktErr != nil {
					reachableIP = false
					logrus.Warnf("vmnet setup failed: %v", sktErr)
				}
			}
		}

		if reachableIP && conf.Kubernetes.Enabled && !ingressIsDisabled(conf.Kubernetes.K3sArgs) {
			out.PortForwards = append(out.PortForwards,
				limaconfig.PortForward{
					GuestIP:           net.IPv4zero,
					GuestPort:         80,
					GuestIPMustBeZero: true,
					Ignore:            true,
					Proto:             limaconfig.TCP,
				},
				limaconfig.PortForward{
					GuestIP:           net.IPv4zero,
					GuestPort:         443,
					GuestIPMustBeZero: true,
					Ignore:            true,
					Proto:             limaconfig.TCP,
				},
			)
		}

		if reachableIP && conf.Runtime == incus.Name {
			out.PortForwards = append(out.PortForwards,
				limaconfig.PortForward{
					GuestIP:           net.IPv4zero,
					GuestIPMustBeZero: true,
					GuestPortRange:    [2]int{1, 65535},
					HostPortRange:     [2]int{1, 65535},
					Ignore:            true,
					Proto:             limaconfig.TCP,
				},
				limaconfig.PortForward{
					GuestIP:        net.ParseIP("127.0.0.1"),
					GuestPortRange: [2]int{1, 65535},
					HostPortRange:  [2]int{1, 65535},
					Ignore:         true,
					Proto:          limaconfig.TCP,
				},
			)
		}
	}

	// --- port forwards and sockets ---

	if conf.Runtime == docker.Name {
		out.PortForwards = append(out.PortForwards,
			limaconfig.PortForward{
				GuestSocket: "/var/run/docker.sock",
				HostSocket:  docker.HostSocketFile(profileConfigDir),
				Proto:       limaconfig.TCP,
			},
			limaconfig.PortForward{
				GuestSocket: "/var/run/containerd/containerd.sock",
				HostSocket:  containerd.HostSocketFiles(profileConfigDir).Containerd,
				Proto:       limaconfig.TCP,
			},
		)

	}

	if conf.Runtime == containerd.Name {
		out.PortForwards = append(out.PortForwards,
			limaconfig.PortForward{
				GuestSocket: "/var/run/containerd/containerd.sock",
				HostSocket:  containerd.HostSocketFiles(profileConfigDir).Containerd,
				Proto:       limaconfig.TCP,
			},
			limaconfig.PortForward{
				GuestSocket: "/var/run/buildkit/buildkitd.sock",
				HostSocket:  containerd.HostSocketFiles(profileConfigDir).Buildkitd,
				Proto:       limaconfig.TCP,
			},
		)
	}

	if conf.Runtime == incus.Name {
		out.PortForwards = append(out.PortForwards, limaconfig.PortForward{
			GuestSocket: "/var/lib/incus/unix.socket",
			HostSocket:  incus.HostSocketFile(profileConfigDir),
			Proto:       limaconfig.TCP,
		})
	}

	// Allow binding on 0.0.0.0
	out.PortForwards = append(out.PortForwards,
		limaconfig.PortForward{
			GuestIPMustBeZero: true,
			GuestIP:           net.IPv4zero,
			GuestPortRange:    [2]int{1, 65535},
			HostIP:            net.IPv4zero,
			HostPortRange:     [2]int{1, 65535},
			Proto:             limaconfig.TCP,
		},
		limaconfig.PortForward{
			GuestIPMustBeZero: true,
			GuestIP:           net.IPv4zero,
			GuestPortRange:    [2]int{1, 65535},
			HostIP:            net.IPv4zero,
			HostPortRange:     [2]int{1, 65535},
			Proto:             limaconfig.UDP,
		},
	)

	// Allow binding on 127.0.0.1
	out.PortForwards = append(out.PortForwards,
		limaconfig.PortForward{
			GuestIP:        net.ParseIP("127.0.0.1"),
			GuestPortRange: [2]int{1, 65535},
			HostIP:         net.ParseIP("127.0.0.1"),
			HostPortRange:  [2]int{1, 65535},
			Proto:          limaconfig.TCP,
		},
		limaconfig.PortForward{
			GuestIP:        net.ParseIP("127.0.0.1"),
			GuestPortRange: [2]int{1, 65535},
			HostIP:         net.ParseIP("127.0.0.1"),
			HostPortRange:  [2]int{1, 65535},
			Proto:          limaconfig.UDP,
		},
	)

	if !conf.Network.Address && conf.Network.HostAddresses {
		for _, ip := range util.LocalIPv4Addrs() {
			out.PortForwards = append(out.PortForwards, limaconfig.PortForward{
				GuestIP:        ip,
				GuestPortRange: [2]int{1, 65535},
				HostIP:         ip,
				HostPortRange:  [2]int{1, 65535},
				Proto:          limaconfig.TCP,
			})
		}
	}

	// --- mount type ---

	switch strings.ToLower(conf.MountType) {
	case "ssh", "sshfs", "reversessh", "reverse-ssh", "reversesshfs", limaconfig.REVSSHFS:
		out.MountType = limaconfig.REVSSHFS
	default:
		if out.VMType == limaconfig.VZ {
			out.MountType = limaconfig.VIRTIOFS
		} else {
			out.MountType = limaconfig.NINEP
		}
	}

	// --- disk provisioning ---

	out.Provision = append(out.Provision, limaconfig.Provision{
		Mode:   limaconfig.ProvisionModeSystem,
		Script: "mount -a",
	})

	if conf.Runtime != incus.Name {
		out.Provision = append(out.Provision, limaconfig.Provision{
			Mode:   limaconfig.ProvisionModeSystem,
			Script: `readlink /usr/sbin/fstrim || fstrim -a`,
		})
	}

	label := volumeLabel(profile.ID)
	out.Provision = append(out.Provision, limaconfig.Provision{
		Mode: limaconfig.ProvisionModeSystem,
		Script: fmt.Sprintf(`DISK=$(blkid -L %s 2>/dev/null || true)
if [ -z "$DISK" ]; then
    echo "Disk %s not found, skipping resize"
    exit 0
fi
resize2fs "$DISK" || true`, label, label),
	})

	// --- mounts ---

	if len(conf.Mounts) == 0 {
		out.Mounts = append(out.Mounts, limaconfig.Mount{Location: "~", Writable: true})
	} else {
		if err = validateMounts(conf.Mounts); err != nil {
			err = fmt.Errorf("overlapping mounts not supported: %w", err)
			return
		}

		out.Mounts = append(out.Mounts, limaconfig.Mount{Location: cacheDir, Writable: false})
		cacheOverlapFound := false

		for _, m := range conf.Mounts {
			var location, mountPoint string
			location, err = util.ResolveMountPath(m.Location)
			if err != nil {
				return
			}
			mountPoint, err = util.ResolveMountPath(m.MountPoint)
			if err != nil {
				return
			}
			out.Mounts = append(out.Mounts, limaconfig.Mount{
				Location:   location,
				MountPoint: mountPoint,
				Writable:   m.Writable,
			})
			if strings.HasPrefix(cacheDir, location) && !cacheOverlapFound {
				out.Mounts = out.Mounts[1:]
				cacheOverlapFound = true
			}
		}
	}

	// --- user-defined provision scripts ---

	for _, script := range conf.Provision {
		out.Provision = append(out.Provision, limaconfig.Provision{
			Mode:   script.Mode,
			Script: script.Script,
		})
	}

	return
}

func resolveMountPath(m domain.Mount) (string, error) {
	if m.MountPoint != "" {
		return util.ResolveMountPath(m.MountPoint)
	}
	return util.ResolveMountPath(m.Location)
}

func validateMounts(mounts []domain.Mount) error {
	for i := 0; i < len(mounts)-1; i++ {
		a, err := resolveMountPath(mounts[i])
		if err != nil {
			return err
		}
		for j := i + 1; j < len(mounts); j++ {
			b, err := resolveMountPath(mounts[j])
			if err != nil {
				return err
			}
			if strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
				return fmt.Errorf("'%s' overlaps '%s'", a, b)
			}
		}
	}
	return nil
}

func ingressIsDisabled(disableFlags []string) bool {
	isDisabled := func(s string) bool { return s == "traefik" || s == "ingress" }
	for i, f := range disableFlags {
		if f == "--disable" {
			if i+1 >= len(disableFlags) {
				return false
			}
			if isDisabled(disableFlags[i+1]) {
				return true
			}
			continue
		}
		parts := strings.SplitN(f, "=", 2)
		if len(parts) < 2 || parts[0] != "--disable" {
			continue
		}
		if isDisabled(parts[1]) {
			return true
		}
	}
	return false
}

const diskLabelMaxLength = 16 // https://tldp.org/HOWTO/Partition/labels.html

func volumeLabel(instanceID string) string {
	name := "lima-" + instanceID
	if len(name) > diskLabelMaxLength {
		name = name[:diskLabelMaxLength]
	}
	return name
}

// isImageVersionAtLeast reports whether a version string (e.g. "26.04") is
// greater than or equal to min (e.g. "26.04").
func isImageVersionAtLeast(version, min string) bool {
	var vmaj, vmin, mmaj, mmin int
	_, _ = fmt.Sscanf(version, "%d.%d", &vmaj, &vmin)
	_, _ = fmt.Sscanf(min, "%d.%d", &mmaj, &mmin)
	return vmaj > mmaj || (vmaj == mmaj && vmin >= mmin)
}
