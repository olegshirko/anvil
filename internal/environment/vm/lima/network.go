package lima

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"anvil/internal/domain"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/util"
)

var defaultLimaNetworkConfig = limautil.LimaNetwork{
	Networks: struct {
		Shared limautil.LimaNetworkConfig `yaml:"user-v2"`
	}{
		Shared: limautil.LimaNetworkConfig{
			Mode:    "user-v2",
			Gateway: net.ParseIP("192.168.5.2"),
			Netmask: "255.255.255.0",
		},
	},
}

func (l *limaVM) writeNetworkFile(conf domain.Config) error {
	netFile := limautil.NetworkFile(l.configService.LimaDir())

	if gw := conf.Network.GatewayAddress; gw != nil {
		defaultLimaNetworkConfig.Networks.Shared.Gateway = gw
	}

	if instances, err := limautil.RunningInstances(); err == nil && len(instances) == 0 {
		if err := os.RemoveAll(limautil.NetworkAssetsDirectory(l.configService.LimaDir())); err != nil {
			logrus.Warnln("cannot clear network assets:", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(netFile), 0755); err != nil {
		return fmt.Errorf("cannot create Lima config directory: %w", err)
	}

	data, err := yaml.Marshal(&defaultLimaNetworkConfig)
	if err != nil {
		return fmt.Errorf("cannot marshal Lima network config: %w", err)
	}

	if err := os.WriteFile(netFile, data, 0755); err != nil {
		return fmt.Errorf("cannot write Lima network config: %w", err)
	}
	return nil
}

func (l *limaVM) replicateHostAddresses(conf domain.Config) error {
	if conf.Network.Address || !conf.Network.HostAddresses {
		return nil
	}
	for _, ip := range util.LocalIPv4Addrs() {
		if err := l.RunQuiet("sudo", "ip", "address", "add", ip.String()+"/24", "dev", "lo"); err != nil {
			return err
		}
	}
	return nil
}

func (l *limaVM) removeHostAddresses() error {
	state, err := limautil.LoadInstance(l.configService.Profile(), l.configService.ProfileStateFile(l.configService.Profile()))
	if err != nil {
		logrus.Tracef("cannot read profile state: %v", err)
		return nil
	}
	if state.Network.Address || !state.Network.HostAddresses {
		return nil
	}
	for _, ip := range util.LocalIPv4Addrs() {
		if err := l.RunQuiet("sudo", "ip", "address", "del", ip.String()+"/24", "dev", "lo"); err != nil {
			return err
		}
	}
	return nil
}
