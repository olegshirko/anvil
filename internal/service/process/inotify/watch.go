package inotify

import (
	"context"
	"fmt"
	"os"

	"anvil/internal/util"

	"github.com/rjeczalik/notify"
	"github.com/sirupsen/logrus"
)

// dirWatcher recursively watches directories and forwards file modification events.
type dirWatcher interface {
	Watch(ctx context.Context, dirs []string, c chan<- fsChange) error
}

type fsNotifyAdapter struct {
	log *logrus.Entry
}

// Watch starts a recursive inotify watcher on the given directories.
func (a *fsNotifyAdapter) Watch(ctx context.Context, dirs []string, events chan<- fsChange) error {
	rawEvents := make(chan notify.EventInfo, 1)

	for _, d := range dirs {
		cleaned, err := util.ResolveMountPath(d)
		if err != nil {
			return fmt.Errorf("invalid watch path %q: %w", d, err)
		}
		if err := notify.Watch(cleaned+"...", rawEvents, notify.Write); err != nil {
			return fmt.Errorf("cannot watch %q: %w", cleaned, err)
		}
	}

	go a.forwardEvents(ctx, rawEvents, events)
	return nil
}

// forwardEvents translates low-level notify events into our fsChange type.
func (a *fsNotifyAdapter) forwardEvents(ctx context.Context, raw chan notify.EventInfo, out chan<- fsChange) {
	log := a.log
	for {
		select {
		case <-ctx.Done():
			notify.Stop(raw)
			log.Trace("stopping inotify watcher")
			if err := ctx.Err(); err != nil {
				log.Tracef("watcher context ended: %v", err)
			}
			return

		case ev := <-raw:
			path := ev.Path()
			log.Tracef("inotify event %s on %s", ev.Event(), path)

			info, err := os.Stat(path)
			if err != nil {
				log.Tracef("cannot stat %q: %v", path, err)
				continue
			}
			if info.IsDir() {
				log.Tracef("skipping directory %q", path)
				continue
			}

			out <- fsChange{filePath: path, mode: info.Mode()}
		}
	}
}

var _ dirWatcher = (*fsNotifyAdapter)(nil)
