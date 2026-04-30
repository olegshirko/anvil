package inotify

import (
	"context"
	"fmt"
	"io/fs"
	"time"
)

// fsChange describes a single filesystem permission change.
type fsChange struct {
	filePath string
	mode     fs.FileMode
}

// Permission returns the octal string representation of the file mode.
func (c fsChange) Permission() string { return fmt.Sprintf("%o", c.mode) }

// watchAndSync runs the filesystem watcher and applies permission fixes as
// containers mount and unmount volumes.
func (w *fsWatcher) watchAndSync(ctx context.Context, watcher dirWatcher) error {
	log := w.log
	log.Trace("starting filesystem watch loop")

	changes := make(chan fsChange)
	mountUpdates := make(chan []string)

	if err := w.trackContainerMounts(ctx, mountUpdates); err != nil {
		return fmt.Errorf("cannot start mount tracker: %w", err)
	}

	defer close(changes)

	var watchedPaths []string
	var stopWatcher context.CancelFunc

	t := &throttle{window: time.Now(), seen: make(map[string]bool)}

	for {
		select {
		case <-ctx.Done():
			if stopWatcher != nil {
				stopWatcher()
			}
			return ctx.Err()

		case paths := <-mountUpdates:
			if sameStrings(paths, watchedPaths) {
				continue
			}
			log.Tracef("mount set changed: %+v → %+v", watchedPaths, paths)
			watchedPaths = paths

			if stopWatcher != nil {
				// graceful restart: give the old watcher a moment to tear down
				go func(cancel context.CancelFunc) {
					time.Sleep(time.Second)
					cancel()
				}(stopWatcher)
			}

			watchCtx, cancel := context.WithCancel(ctx)
			stopWatcher = cancel
			go w.runWatcher(watchCtx, watcher, watchedPaths, changes)

		case ev := <-changes:
			if !t.allow(ev.filePath) {
				continue
			}
			if err := w.syncPermission(ev); err != nil {
				log.Trace(err)
			}
		}
	}
}

// runWatcher is a goroutine wrapper around the underlying dirWatcher.
func (w *fsWatcher) runWatcher(ctx context.Context, watcher dirWatcher, paths []string, out chan<- fsChange) {
	if err := watcher.Watch(ctx, paths, out); err != nil {
		w.log.Errorf("watcher exited: %v", err)
	}
}

// syncPermission applies the detected permission to the file inside the VM.
func (w *fsWatcher) syncPermission(ev fsChange) error {
	log := w.log
	if err := w.guest.RunQuiet("stat", ev.filePath); err != nil {
		return fmt.Errorf("cannot stat %q: %w", ev.filePath, err)
	}
	log.Infof("syncing permission for %s", ev.filePath)
	if err := w.guest.RunQuiet("sudo", "/bin/chmod", ev.Permission(), ev.filePath); err != nil {
		return fmt.Errorf("cannot chmod %q: %w", ev.filePath, err)
	}
	return nil
}

// throttle implements a simple rate-limiter: at most 50 unique paths per
// 500 ms window.
type throttle struct {
	window time.Time
	seen   map[string]bool
}

func (t *throttle) allow(path string) bool {
	now := time.Now()
	if now.Sub(t.window) >= 500*time.Millisecond {
		t.window = now
		t.seen = make(map[string]bool)
	}
	if t.seen[path] {
		return false
	}
	if len(t.seen) >= 50 {
		return false
	}
	t.seen[path] = true
	return true
}

// sameStrings reports whether two slices are identical in order and contents.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
