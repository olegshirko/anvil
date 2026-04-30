package kubernetes

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"anvil/internal/cli"
	"anvil/internal/environment"
	"anvil/internal/environment/container/containerd"
	"anvil/internal/environment/container/docker"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/util"
	"anvil/internal/util/downloader"

	"github.com/sirupsen/logrus"
)

const listenPortKey = "k3s_listen_port"

// argExists reports whether the k3s argument list already contains the given flag.
func argExists(args []string, name string) bool {
	prefixEq := name + "="
	prefixSp := name + " "
	for _, a := range args {
		if a == name || strings.HasPrefix(a, prefixEq) || strings.HasPrefix(a, prefixSp) {
			return true
		}
	}
	return false
}

// provisionK3s orchestrates downloading, caching and installing k3s.
func provisionK3s(
	host environment.HostActions,
	guest environment.GuestActions,
	pipe *cli.ActiveCommandChain,
	log *logrus.Entry,
	containerRuntime string,
	k3sVersion string,
	k3sArgs []string,
	k3sListenPort int,
	profileID string,
	cacheDir string,
) {
	arch := guest.Arch().GoArch()
	baseURL := "https://github.com/k3s-io/k3s/releases/download/" + k3sVersion + "/"
	shaFile := "sha256sum-" + arch + ".txt"
	shaURL := baseURL + shaFile

	binaryURL := baseURL + "k3s"
	if arch == "arm64" {
		binaryURL += "-arm64"
	}

	imageName := "k3s-airgap-images-" + arch + ".tar"
	imageURL := baseURL + imageName + ".gz"

	var wg sync.WaitGroup
	var binErr, imgErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, binErr = downloader.Download(host, log.Logger, cacheDir, downloader.Request{
			URL: binaryURL,
			SHA: &downloader.SHA{Size: 256, URL: shaURL},
		})
	}()
	go func() {
		defer wg.Done()
		_, imgErr = downloader.Download(host, log.Logger, cacheDir, downloader.Request{
			URL: imageURL,
			SHA: &downloader.SHA{Size: 256, URL: shaURL},
		})
	}()
	wg.Wait()

	if binErr != nil {
		pipe.Add(func() error { return fmt.Errorf("cannot download k3s binary: %w", binErr) })
	}
	if imgErr != nil {
		pipe.Add(func() error { return fmt.Errorf("cannot download k3s air-gap images: %w", imgErr) })
	}
	if binErr != nil || imgErr != nil {
		return
	}

	stageK3sBinary(host, guest, pipe, log.Logger, k3sVersion, cacheDir)
	stageAirGapImages(host, guest, pipe, log, k3sVersion, cacheDir)
	bootstrapCluster(host, guest, pipe, containerRuntime, k3sVersion, k3sArgs, k3sListenPort, profileID, cacheDir)
}

// stageK3sBinary copies the k3s binary into the guest VM.
func stageK3sBinary(
	host environment.HostActions,
	guest environment.GuestActions,
	pipe *cli.ActiveCommandChain,
	log *logrus.Logger,
	version string,
	cacheDir string,
) {
	tmpPath := "/tmp/k3s"
	arch := guest.Arch().GoArch()
	base := "https://github.com/k3s-io/k3s/releases/download/" + version + "/"
	sha := "sha256sum-" + arch + ".txt"
	url := base + "k3s"
	if arch == "arm64" {
		url += "-arm64"
	}

	pipe.Add(func() error {
		return downloader.DownloadToGuest(host, guest, log, cacheDir, downloader.Request{
			URL: url,
			SHA: &downloader.SHA{Size: 256, URL: base + sha},
		}, tmpPath)
	})
	pipe.Add(func() error {
		return guest.Run("sudo", "install", tmpPath, "/usr/local/bin/k3s")
	})
}

// stageAirGapImages downloads and places k3s air-gap images on the guest.
func stageAirGapImages(
	host environment.HostActions,
	guest environment.GuestActions,
	pipe *cli.ActiveCommandChain,
	log *logrus.Entry,
	version string,
	cacheDir string,
) {
	arch := guest.Arch().GoArch()
	base := "https://github.com/k3s-io/k3s/releases/download/" + version + "/"
	imageName := "k3s-airgap-images-" + arch + ".tar"
	sha := "sha256sum-" + arch + ".txt"
	tmpGz := "/tmp/" + imageName + ".gz"
	tmpTar := "/tmp/" + imageName

	pipe.Add(func() error {
		return downloader.DownloadToGuest(host, guest, log.Logger, cacheDir, downloader.Request{
			URL: base + imageName + ".gz",
			SHA: &downloader.SHA{Size: 256, URL: base + sha},
		}, tmpGz)
	})
	pipe.Add(func() error {
		return guest.Run("gzip", "-f", "-d", tmpGz)
	})

	imgDir := "/var/lib/rancher/k3s/agent/images/"
	pipe.Add(func() error {
		return guest.Run("sudo", "mkdir", "-p", imgDir)
	})
	pipe.Add(func() error {
		return guest.Run("sudo", "cp", tmpTar, imgDir)
	})
}

// bootstrapCluster runs the k3s installer on the guest with the generated arguments.
func bootstrapCluster(
	host environment.HostActions,
	guest environment.GuestActions,
	pipe *cli.ActiveCommandChain,
	containerRuntime string,
	version string,
	k3sArgs []string,
	k3sListenPort int,
	profileID string,
	cacheDir string,
) {
	installScript := "/tmp/k3s-install.sh"
	scriptURL := "https://raw.githubusercontent.com/k3s-io/k3s/" + version + "/install.sh"

	pipe.Add(func() error {
		if guest.RunQuiet("stat", "/usr/local/bin/k3s-install.sh") == nil {
			return nil
		}
		return downloader.DownloadToGuest(host, guest, logrus.StandardLogger(), cacheDir, downloader.Request{URL: scriptURL}, installScript)
	})
	pipe.Add(func() error {
		if guest.RunQuiet("stat", "/usr/local/bin/k3s-install.sh") == nil {
			return nil
		}
		return guest.Run("sudo", "install", installScript, "/usr/local/bin/k3s-install.sh")
	})

	args := append([]string{"--write-kubeconfig-mode", "644"}, k3sArgs...)

	pipe.Retry("waiting for VM IP address", time.Second*2, 10, func(retryCount int) error {
		ip := limautil.NewNetworkInspector(profileID).FindIP()
		if ip == "" {
			return fmt.Errorf("VM has no assigned IP address")
		}
		if ip == "127.0.0.1" {
			args = append(args, "--flannel-iface", "eth0")
		} else {
			if !argExists(k3sArgs, "--advertise-address") {
				args = append(args, "--advertise-address", ip)
			}
			if !argExists(k3sArgs, "--flannel-iface") {
				args = append(args, "--flannel-iface", limautil.DefaultInterfaceName)
			}
		}
		return nil
	})

	switch containerRuntime {
	case docker.Name:
		args = append(args, "--docker")
	case containerd.Name:
		args = append(args, "--container-runtime-endpoint", "unix:///run/containerd/containerd.sock")
	}

	pipe.Add(func() error {
		port, err := resolveAPIPort(guest, k3sListenPort)
		if err != nil {
			return err
		}
		args = append(args, "--https-listen-port", strconv.Itoa(port))
		return nil
	})

	pipe.Add(func() error {
		return guest.Run("sh", "-c", "INSTALL_K3S_SKIP_DOWNLOAD=true INSTALL_K3S_SKIP_ENABLE=true k3s-install.sh "+strings.Join(args, " "))
	})
}

// resolveAPIPort returns the previously persisted API port or assigns a new one.
func resolveAPIPort(guest environment.GuestActions, preferred int) (int, error) {
	if saved, err := strconv.Atoi(guest.Setting(listenPortKey)); err == nil && saved > 0 {
		return saved, nil
	}
	if guest.Setting(k3sMasterIPKey) != "" {
		return 6443, nil
	}
	port := preferred
	if port <= 0 {
		var err error
		port, err = util.FreeTCPPort()
		if err != nil {
			return 0, fmt.Errorf("cannot allocate random port: %w", err)
		}
	}
	if err := guest.SetSetting(listenPortKey, strconv.Itoa(port)); err != nil {
		return 0, err
	}
	return port, nil
}
