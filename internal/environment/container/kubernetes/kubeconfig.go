package kubernetes

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"anvil/internal/cli"
	"anvil/internal/domain"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/usecase"
)

const k3sMasterIPKey = "master_address"

// syncKubeconfig merges the VM's k3s kubeconfig into the host's kubeconfig.
func (k k3sEngine) syncKubeconfig(ctx context.Context, cfg domain.Config) error {
	id := k.profileID
	masterIP := limautil.NewNetworkInspector(id).FindIP()
	if masterIP == k.guest.Setting(k3sMasterIPKey) {
		return nil
	}

	log := k.Logger(ctx)
	pipe := k.Init(ctx)
	pipe.Stage("updating kubeconfig")

	k.clearKubeconfigEntries(pipe, id)

	home, err := k.host.Env("HOME")
	if err != nil {
		return fmt.Errorf("cannot read host HOME: %w", err)
	}
	if home == "" {
		return fmt.Errorf("host HOME is empty")
	}
	kubeDir := filepath.Join(home, ".kube")
	pipe.Add(func() error {
		return k.host.Run("mkdir", "-p", filepath.Join(kubeDir, "."+id))
	})

	mainConfig := filepath.Join(kubeDir, "config")
	if envPath, _ := k.host.Env("KUBECONFIG"); envPath != "" {
		mainConfig = filepath.SplitList(envPath)[0]
	}
	tempConfig := filepath.Join(kubeDir, "."+id, "anvil-temp")

	// fetch and patch k3s yaml inside the VM
	pipe.Add(func() error {
		raw, err := k.guest.Read("/etc/rancher/k3s/k3s.yaml")
		if err != nil {
			return fmt.Errorf("cannot read k3s.yaml: %w", err)
		}
		raw = strings.ReplaceAll(raw, ": default", ": "+id)
		if masterIP != "" && masterIP != "127.0.0.1" {
			raw = strings.ReplaceAll(raw, "https://127.0.0.1:", "https://"+masterIP+":")
		}
		return k.host.Write(tempConfig, []byte(raw))
	})

	// merge with existing host config
	pipe.Add(func() error {
		host := k.host.WithEnv(fmt.Sprintf("KUBECONFIG=%s:%s", mainConfig, tempConfig))
		merged, err := host.RunOutput("kubectl", "config", "view", "--raw")
		if err != nil {
			return err
		}
		return host.Write(tempConfig, []byte(merged))
	})

	// backup current and atomically replace
	pipe.Add(func() error {
		if stat, err := k.host.Stat(mainConfig); err == nil && !stat.IsDir() {
			bak := filepath.Join(filepath.Dir(tempConfig), fmt.Sprintf("config-bak-%d", time.Now().Unix()))
			if err := k.host.Run("cp", mainConfig, bak); err != nil {
				return fmt.Errorf("cannot backup kubeconfig: %w", err)
			}
		}
		if err := k.host.Run("cp", tempConfig, mainConfig); err != nil {
			return fmt.Errorf("cannot update kubeconfig: %w", err)
		}
		return nil
	})

	if usecase.ConfigAutoActivate(cfg) {
		pipe.Add(func() error {
			out, err := k.host.RunOutput("kubectl", "config", "use-context", id)
			if err != nil {
				return err
			}
			log.Println(out)
			return nil
		})
	}

	pipe.Add(func() error {
		return k.guest.SetSetting(k3sMasterIPKey, masterIP)
	})

	return pipe.Exec()
}

// clearKubeconfigEntries removes all references to the profile from kubectl config.
func (k k3sEngine) clearKubeconfigEntries(pipe *cli.ActiveCommandChain, id string) {
	for _, section := range []string{"users", "contexts", "clusters"} {
		key := section + "." + id
		pipe.Add(func() error {
			return k.host.Run("kubectl", "config", "unset", key)
		})
	}
	pipe.Add(func() error {
		if current, _ := k.host.RunOutput("kubectl", "config", "current-context"); current != id {
			return nil
		}
		return k.host.Run("kubectl", "config", "unset", "current-context")
	})
}

// resetKubeconfig removes the profile from kubeconfig and clears the stored IP.
func (k k3sEngine) resetKubeconfig(pipe *cli.ActiveCommandChain) {
	pipe.Stage("reverting kubeconfig")
	k.clearKubeconfigEntries(pipe, k.profileID)
	pipe.Add(func() error {
		return k.guest.SetSetting(k3sMasterIPKey, "")
	})
}
