package incus

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"anvil/internal/cli"
	"anvil/internal/domain"
	"anvil/internal/environment"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/usecase"
	"anvil/internal/util/debutil"
)

const incusBridgeInterface = "incusbr0"

const (
	Name = "incus"

	storageDriver = "zfs"

	poolName    = "default"
	poolMetaDir = "/var/lib/incus/storage-pools/" + poolName

	poolDisksDir = "/var/lib/incus/disks"
	poolDiskFile = poolDisksDir + "/" + poolName + ".img"
)

func createIncusEngine(host environment.HostActions, guest environment.GuestActions, profileConfigDir string, profileID string, cacheDir string) environment.ContainerRuntime {
	storeDir, _ := os.UserHomeDir()
	storeDir = filepath.Join(storeDir, ".local", "share", "anvil")
	return &incusEngine{
		host:             host,
		guest:            guest,
		profileConfigDir: profileConfigDir,
		profileID:        profileID,
		storeFile:        filepath.Join(storeDir, profileID+".json"),
		CommandChain:     cli.New(Name),
	}
}

func init() {
	environment.RegisterRuntime(Name, createIncusEngine, false)
}

var _ environment.ContainerRuntime = (*incusEngine)(nil)

type incusEngine struct {
	host             environment.HostActions
	guest            environment.GuestActions
	profileConfigDir string
	profileID        string
	storeFile        string
	cli.CommandChain
}

func (i incusEngine) Name() string { return Name }

func (i *incusEngine) Dependencies() []string {
	return []string{"incus"}
}

// Provision sets up Incus storage and networking.
func (i *incusEngine) Provision(ctx context.Context, conf domain.Config) error {
	log := i.Logger(ctx)

	if found, _, _ := i.lookupNetwork(incusBridgeInterface); found {
		return nil
	}

	emptyDisk := true
	recoverStorage := false
	if limautil.IsDiskReady(i.profileID, Name, i.storeFile) {
		emptyDisk = false
		recoverStorage = cli.ConfirmPrompt("existing Incus data found, would you like to recover the storage pool(s)")
	}

	var params struct {
		Disk       int
		Interface  string
		SetStorage bool
	}
	params.Disk = conf.Disk
	params.Interface = incusBridgeInterface
	params.SetStorage = emptyDisk

	var buf bytes.Buffer
	if err := incusConfigTmpl.Execute(&buf, params); err != nil {
		return fmt.Errorf("cannot render incus config: %w", err)
	}

	stdin := bytes.NewReader(buf.Bytes())
	if err := i.guest.RunWith(stdin, nil, "sudo", "incus", "admin", "init", "--preseed"); err != nil {
		return fmt.Errorf("cannot initialize incus: %w", err)
	}

	if emptyDisk {
		return nil
	}

	if !recoverStorage {
		return i.resetStorage(conf.Disk)
	}

	if _, err := i.guest.Stat(poolDiskFile); err != nil {
		log.Warnln(fmt.Errorf("cannot recover disk: %v, creating new storage pool", err))
		return i.resetStorage(conf.Disk)
	}

	for {
		if err := i.restorePools(ctx); err != nil {
			log.Warnln(err)
			if cli.ConfirmPrompt("recovery failed for default storage pool, try again") {
				continue
			}
			log.Warnln("discarding disk, creating new storage pool")
			return i.resetStorage(conf.Disk)
		}
		break
	}

	return nil
}

func (i incusEngine) Running(ctx context.Context) bool {
	return i.guest.RunQuiet("service", "incus", "status") == nil
}

// Start ensures the Incus daemon is running and configures remotes.
func (i incusEngine) Start(ctx context.Context, conf domain.Config) error {
	pipe := i.Init(ctx)

	if i.poolIsLoaded() {
		pipe.Add(func() error {
			return i.guest.RunQuiet("sudo", "systemctl", "start", "incus.service")
		})
	} else {
		pipe.Add(func() error {
			return i.guest.RunQuiet("sudo", "systemctl", "restart", "incus.service")
		})
	}

	if conf.Disk > 0 {
		log := i.Logger(ctx)
		pipe.Add(func() error {
			if err := i.guest.RunQuiet("sudo", "incus", "storage", "set", "default", "size="+usecase.DiskGiB(domain.Disk(conf.Disk))); err != nil {
				log.Traceln("error syncing incus storage size:", err)
			}
			return nil
		})
	}

	pipe.Add(func() error {
		if err := i.configureRemote(usecase.ConfigAutoActivate(conf)); err == nil {
			return nil
		}
		ctx := context.WithValue(ctx, cli.CtxKeyQuiet, true)
		if err := i.guest.Restart(ctx); err != nil {
			return err
		}
		return i.configureRemote(usecase.ConfigAutoActivate(conf))
	})

	pipe.Add(func() error {
		if err := i.enableDockerHub(); err != nil {
			return cli.MarkNonFatal(err)
		}
		return nil
	})

	pipe.Add(func() error {
		if err := i.setupNetworks(); err != nil {
			return cli.MarkNonFatal(err)
		}
		return nil
	})

	return pipe.Exec()
}

func (i incusEngine) Stop(ctx context.Context) error {
	pipe := i.Init(ctx)
	pipe.Add(func() error {
		return i.guest.RunQuiet("sudo", "incus", "admin", "shutdown")
	})
	pipe.Add(i.removeRemote)
	return pipe.Exec()
}

func (i incusEngine) Teardown(ctx context.Context) error {
	pipe := i.Init(ctx)
	pipe.Add(i.removeRemote)
	return pipe.Exec()
}

func (i incusEngine) Version(ctx context.Context, _ string) string {
	v, _ := i.host.RunOutput("incus", "version", i.profileID+":")
	return v
}

func (i *incusEngine) Update(ctx context.Context) (bool, error) {
	packages := []string{
		"incus",
		"incus-base",
		"incus-client",
		"incus-extra",
		"incus-ui-canonical",
	}
	return debutil.EnsurePackages(ctx, i.guest, i, "incus", packages...)
}

func (i incusEngine) Host() environment.HostActions   { return i.host }
func (i incusEngine) Guest() environment.GuestActions { return i.guest }

// HostSocketFile is the Unix socket path exposed on the host.
func HostSocketFile(profileConfigDir string) string {
	return filepath.Join(profileConfigDir, "incus.sock")
}

func (i incusEngine) configureRemote(activate bool) error {
	name := i.profileID
	if !i.remoteExists(name) {
		if err := i.host.RunQuiet("incus", "remote", "add", name, "unix://"+HostSocketFile(i.profileConfigDir)); err != nil {
			return err
		}
	}
	if activate {
		return i.host.RunQuiet("incus", "remote", "switch", name)
	}
	return nil
}

func (i incusEngine) removeRemote() error {
	if i.isActiveRemote() {
		if err := i.host.RunQuiet("incus", "remote", "switch", "local"); err != nil {
			return err
		}
	}
	if i.remoteExists(i.profileID) {
		return i.host.RunQuiet("incus", "remote", "remove", i.profileID)
	}
	return nil
}

func (i incusEngine) remoteExists(name string) bool {
	remotes, err := i.listRemotes()
	if err != nil {
		return false
	}
	_, ok := remotes[name]
	return ok
}

func (i incusEngine) listRemotes() (remoteInfo, error) {
	b, err := i.host.RunOutput("incus", "remote", "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("cannot list remotes: %w", err)
	}
	var remotes remoteInfo
	if err := json.NewDecoder(strings.NewReader(b)).Decode(&remotes); err != nil {
		return nil, fmt.Errorf("cannot decode remotes: %w", err)
	}
	return remotes, nil
}

func (i incusEngine) isActiveRemote() bool {
	remote, _ := i.host.RunOutput("incus", "remote", "get-default")
	return remote == i.profileID
}

func (i incusEngine) enableDockerHub() error {
	if i.remoteExists("docker") {
		return nil
	}
	return i.host.RunQuiet("incus", "remote", "add", "docker", "https://docker.io", "--protocol=oci")
}

func (i incusEngine) setupNetworks() error {
	name := limautil.DefaultInterfaceName
	found, netw, err := i.lookupNetwork(name)
	if err != nil {
		return err
	}
	if !found || netw.Managed || netw.Type != "physical" {
		return nil
	}
	if err := i.guest.RunQuiet("sudo", "incus", "network", "create", name, "--type", "macvlan", "parent="+name); err != nil {
		return fmt.Errorf("cannot create managed network '%s': %w", name, err)
	}
	return nil
}

func (i incusEngine) lookupNetwork(interfaceName string) (bool, networkInfo, error) {
	b, err := i.guest.RunOutput("sudo", "incus", "network", "list", "--format", "json")
	if err != nil {
		return false, networkInfo{}, fmt.Errorf("cannot list networks: %w", err)
	}
	var resp []networkInfo
	if err := json.NewDecoder(strings.NewReader(b)).Decode(&resp); err != nil {
		return false, networkInfo{}, fmt.Errorf("cannot decode networks: %w", err)
	}
	for _, n := range resp {
		if n.Name == interfaceName {
			return true, n, nil
		}
	}
	return false, networkInfo{}, nil
}

func (i *incusEngine) poolIsLoaded() bool {
	script := strings.NewReplacer(
		"{pool_name}", poolName,
	).Replace("sudo zpool list -H -o name | grep '^{pool_name}$'")
	return i.guest.RunQuiet("sh", "-c", script) == nil
}

func (i *incusEngine) restorePools(ctx context.Context) error {
	str, err := i.guest.RunOutput("sh", "-c", "sudo ls "+poolDisksDir+" | grep '.img$'")
	if err != nil {
		return fmt.Errorf("cannot list storage pool disks: %w", err)
	}

	disks := strings.Fields(str)
	if len(disks) == 0 {
		return fmt.Errorf("no existing storage pool disks found")
	}

	log := i.Logger(ctx)
	log.Println()
	log.Println("Running 'incus admin recover' ...")
	log.Println()
	log.Println(fmt.Sprintf("Found %d storage pool source(s):", len(disks)))
	for _, disk := range disks {
		log.Println("  " + poolDisksDir + "/" + disk)
	}
	log.Println()

	if err := i.guest.RunInteractive("sudo", "incus", "admin", "recover"); err != nil {
		return fmt.Errorf("recovery failed: %w", err)
	}

	out, err := i.guest.RunOutput("sudo", "incus", "storage", "list", "name="+poolName, "-c", "n", "--format", "compact,noheader")
	if err != nil {
		return err
	}
	if out != poolName {
		return fmt.Errorf("default storage pool recovery failure")
	}
	return nil
}

func (i *incusEngine) resetStorage(size int) error {
	deleteScript := strings.NewReplacer(
		"{disk_file}", poolDiskFile,
		"{meta_dir}", poolMetaDir,
	).Replace("sudo rm -rf {disk_file} {meta_dir}")

	if err := i.guest.RunQuiet("sh", "-c", deleteScript); err != nil {
		return fmt.Errorf("cannot prepare storage pools directory: %w", err)
	}

	diskSize := fmt.Sprintf("%dGiB", size)
	return i.guest.RunQuiet("sudo", "incus", "storage", "create", poolName, storageDriver, "size="+diskSize)
}

// DataDisk describes the directories Incus places on the external data disk.
func DataDisk() environment.DataDisk {
	return environment.DataDisk{
		FSType: "ext4",
		Dirs: []environment.DiskDir{
			{Name: "incus-disks", Path: "/var/lib/incus/disks"},
			{Name: "incus-backups", Path: "/var/lib/incus/backups"},
		},
	}
}

//go:embed config.yaml
var configYaml string

var incusConfigTmpl = template.Must(template.New("incus-config").Parse(configYaml))

type remoteInfo map[string]struct {
	Addr string `json:"Addr"`
}

type networkInfo struct {
	Name    string `json:"name"`
	Managed bool   `json:"managed"`
	Type    string `json:"type"`
}
