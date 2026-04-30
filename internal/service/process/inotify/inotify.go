package inotify

import (
	"context"
	"fmt"
	"time"

	"anvil/internal/environment"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/service/process"

	"github.com/sirupsen/logrus"
)

const Name = "inotify"
const volumesInterval = 5 * time.Second

type Args struct {
	environment.GuestActions
	Dirs      []string
	Runtime   string
	ProfileID string
}

// ArgsContextKey returns the context key used to pass inotify arguments.
func ArgsContextKey() any { return struct{ name string }{name: "inotify_args"} }

// New creates a new inotify background process.
func New() process.Process {
	return &fsWatcher{
		log: logrus.WithField("context", "inotify"),
	}
}

var _ process.Process = (*fsWatcher)(nil)

type fsWatcher struct {
	vmVols    []string
	guest     environment.GuestActions
	runtime   string
	profileID string
	log       *logrus.Entry
}

// Alive reports whether the inotify watcher is active.
func (w *fsWatcher) Alive(ctx context.Context) error {
	if running, _ := ctx.Value(process.CtxKeyDaemon()).(bool); running {
		return nil
	}
	return fmt.Errorf("inotify not running")
}

// Dependencies implements process.Process.
func (*fsWatcher) Dependencies() (deps []process.Dependency, root bool) {
	return nil, false
}

// Name implements process.Process.
func (*fsWatcher) Name() string {
	return Name
}

// Start begins watching for filesystem events.
func (w *fsWatcher) Start(ctx context.Context) error {
	args, ok := ctx.Value(ArgsContextKey()).(Args)
	if !ok {
		return fmt.Errorf("inotify args missing in context")
	}
	w.vmVols = pruneSubpaths(args.Dirs)
	w.guest = args.GuestActions
	w.runtime = args.Runtime
	w.profileID = args.ProfileID
	log := w.log

	log.Info("waiting for VM to start")
	w.awaitVMReady(ctx)
	log.Info("VM started")

	watcher := &fsNotifyAdapter{log: log}
	return w.watchAndSync(ctx, watcher)
}

// awaitVMReady blocks until the Lima instance is running and reachable.
func (w *fsWatcher) awaitVMReady(ctx context.Context) {
	log := w.log
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	for {
		log.Info("waiting 5s for VM")
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			inst, err := limautil.Instance(w.profileID)
			if err != nil || !inst.Running() {
				continue
			}
			if err := w.guest.RunQuiet("uname", "-a"); err == nil {
				return
			}
		}
	}
}
