package lima

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"anvil/internal/domain"
	"anvil/internal/environment"
	"anvil/internal/environment/container/containerd"
	"anvil/internal/environment/container/docker"
	"anvil/internal/environment/container/incus"
	"anvil/internal/environment/vm/lima/limaconfig"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/store"
	"anvil/internal/util/downloader"
)

//go:embed disk.sh
var diskScript string

func runtimeDataDisk(runtime string) environment.DataDisk {
	switch runtime {
	case docker.Name:
		return docker.DataDisk()
	case containerd.Name:
		return containerd.DataDisk()
	case incus.Name:
		return incus.DataDisk()
	}
	return environment.DataDisk{}
}

func (l *limaVM) createRuntimeDisk(conf domain.Config) error {
	if environment.RuntimeIsNone(conf.Runtime) || conf.Disk == 0 {
		return nil
	}

	storeFile := l.configService.ProfileStoreFile(l.configService.Profile())
	disk := runtimeDataDisk(conf.Runtime)
	state, _ := store.Fetch(storeFile)
	needsFormat := !state.DiskFormatted

	profileID := l.configService.Profile().ID
	if !limautil.DiskExists(profileID) {
		if err := limautil.AllocateDisk(profileID, conf.Disk); err != nil {
			return fmt.Errorf("cannot create runtime disk: %w", err)
		}
		needsFormat = true
	}

	if state.DiskFormatted && state.DiskRuntime != "" && state.DiskRuntime != conf.Runtime {
		return fmt.Errorf("disk already provisioned for %s; delete data with 'anvil delete --data' first", state.DiskRuntime)
	}

	l.limaConf.Disk = domain.Disk(conf.RootDisk).GiB()
	l.limaConf.AdditionalDisks = append(l.limaConf.AdditionalDisks, limaconfig.Disk{
		Name:   profileID,
		Format: needsFormat,
		FSType: disk.FSType,
	})

	return l.attachDisk(profileID, conf, needsFormat)
}

func (l *limaVM) useRuntimeDisk(conf domain.Config) error {
	if conf.Disk == 0 {
		l.limaConf.Disk = domain.Disk(conf.RootDisk).GiB()
		return nil
	}

	profileID := l.configService.Profile().ID
	if !limautil.DiskExists(profileID) {
		l.limaConf.Disk = domain.Disk(conf.Disk).GiB()
		return nil
	}

	storeFile := l.configService.ProfileStoreFile(l.configService.Profile())
	disk := runtimeDataDisk(conf.Runtime)
	state, _ := store.Fetch(storeFile)
	needsFormat := !state.DiskFormatted

	l.limaConf.Disk = domain.Disk(conf.RootDisk).GiB()
	l.limaConf.AdditionalDisks = append(l.limaConf.AdditionalDisks, limaconfig.Disk{
		Name:   profileID,
		Format: needsFormat,
		FSType: disk.FSType,
	})

	return l.attachDisk(profileID, conf, needsFormat)
}

func renderDiskScript(format bool, instanceID string) (string, error) {
	vals := struct {
		Format     bool
		InstanceId string
	}{Format: format, InstanceId: instanceID}

	tmpl, err := template.New("disk").Parse(diskScript)
	if err != nil {
		return "", fmt.Errorf("cannot parse disk script: %w", err)
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, vals); err != nil {
		return "", fmt.Errorf("cannot execute disk script: %w", err)
	}
	return b.String(), nil
}

func (l *limaVM) attachDisk(profileID string, conf domain.Config, format bool) error {
	script, err := renderDiskScript(format, profileID)
	if err != nil {
		return err
	}

	l.limaConf.Provision = append(l.limaConf.Provision, limaconfig.Provision{
		Mode:   "dependency",
		Script: script,
	})

	disk := runtimeDataDisk(conf.Runtime)
	for _, pre := range disk.PreMount {
		l.limaConf.Provision = append(l.limaConf.Provision, limaconfig.Provision{
			Mode:   "dependency",
			Script: pre,
		})
	}

	mp := limautil.DiskMountPath(profileID)
	for _, dir := range disk.Dirs {
		s := strings.NewReplacer(
			"{mount_point}", mp,
			"{name}", dir.Name,
			"{data_path}", dir.Path,
		).Replace("[ -d {mount_point} ] && mkdir -p {mount_point}/{name} {data_path} && mount --bind {mount_point}/{name} {data_path}")
		l.limaConf.Provision = append(l.limaConf.Provision, limaconfig.Provision{
			Mode:   "dependency",
			Script: s,
		})
	}
	return nil
}

func (l *limaVM) downloadDiskImage(conf domain.Config) error {
	arch := environment.Arch(conf.Arch).Value()

	if conf.DiskImage != "" {
		if _, err := os.Stat(conf.DiskImage); err != nil {
			return fmt.Errorf("invalid disk image: %w", err)
		}
		img, err := limautil.Image(arch, conf.Runtime, conf.ImageVersion)
		if err != nil {
			return fmt.Errorf("cannot resolve disk image: %w", err)
		}
		sha := downloader.SHA{Size: 512, Digest: img.Digest}
		if err := sha.ValidateFile(l.host, conf.DiskImage); err != nil {
			return fmt.Errorf("disk image hash mismatch against '%s': %w", img.Location, err)
		}
		img.Location = conf.DiskImage
		l.limaConf.Images = []limaconfig.File{img}
		l.setKernelImage()
		return nil
	}

	if img, ok := limautil.ImageCached(arch, conf.Runtime, conf.ImageVersion, l.configService.CacheDir()); ok {
		l.limaConf.Images = []limaconfig.File{img}
		l.setKernelImage()
		return nil
	}

	img, err := limautil.DownloadImage(arch, conf.Runtime, conf.ImageVersion, l.configService.CacheDir())
	if err != nil {
		return fmt.Errorf("cannot download disk image: %w", err)
	}
	l.limaConf.Images = []limaconfig.File{img}
	l.setKernelImage()
	return nil
}

func (l *limaVM) downloadKernel(runtime string, arch environment.Arch, version string) {
	path, err := limautil.DownloadKernel(arch, runtime, version, l.configService.CacheDir())
	if err != nil {
		logrus.WithError(err).Warnf("kernel download failed for %s", runtime)
		return
	}
	logrus.Infof("kernel downloaded for %s: %s", runtime, path)
	l.kernelPath = path
}

func (l *limaVM) setKernelImage() {
	if l.kernelPath == "" || len(l.limaConf.Images) == 0 {
		return
	}
	l.limaConf.Images[0].Kernel = &limaconfig.FileKernel{
		Location: l.kernelPath,
		Cmdline:  "root=/dev/vda1 rw console=ttyS0 quiet",
	}
}

func (l *limaVM) setDiskImage() error {
	var c limaconfig.Config
	b, err := os.ReadFile(l.configService.ProfileLimaFile(l.configService.Profile()))
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return err
	}
	l.limaConf.Images = c.Images
	return nil
}

func (l *limaVM) syncDiskSize(ctx context.Context, conf domain.Config) domain.Config {
	log := l.Logger(ctx)
	current, err := limautil.FetchDiskSize(l.configService.Profile().ID)
	if err != nil {
		return conf
	}

	resized := false
	if current != conf.Disk {
		if conf.Disk < current {
			log.Warnln("disk shrink not supported, ignoring...")
		} else {
			if err := limautil.ExpandDisk(l.configService.Profile().ID, conf.Disk); err != nil {
				log.Warnln(fmt.Errorf("disk resize failed: %v", err))
			} else {
				log.Printf("disk resized to %dGiB", conf.Disk)
				resized = true
			}
		}
	}

	if !resized && conf.Disk != current {
		conf.Disk = current
		if err := l.configService.SaveConfig(ctx, conf); err != nil {
			log.Warnln(fmt.Errorf("cannot persist corrected disk size: %v", err))
		}
	}

	return conf
}
