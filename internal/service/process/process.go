package process

import (
	"context"
	"fmt"

	"anvil/internal/environment"
)

// daemonKey is the typed context key for daemon-scoped values.
type daemonKey struct{ name string }

// CtxKeyDaemon returns a unique context key for daemon arguments.
func CtxKeyDaemon() any { return daemonKey{name: "anvil_daemon"} }

// Process is a background process managed by the daemon.
type Process interface {
	Name() string
	Start(ctx context.Context) error
	Alive(ctx context.Context) error
	Dependencies() (deps []Dependency, root bool)
}

// Dependency is a requirement to be fulfilled before a process can be started.
type Dependency interface {
	Present() bool
	Apply(environment.HostActions) error
}

var globalDaemonDir string

// SetDir configures the directory used for daemon pid/log files.
func SetDir(dir string) { globalDaemonDir = dir }

// Dir returns the currently configured daemon directory.
func Dir() string { return globalDaemonDir }

// Requirements aggregates dependencies for a set of processes.
type Requirements struct {
	needsRoot bool
	items     []installTask
}

type installTask struct {
	proc string
	dep  Dependency
}

// Resolve collects all missing dependencies for the given processes.
func Resolve(processes ...Process) *Requirements {
	r := &Requirements{}
	seen := make(map[Dependency]bool)

	for _, proc := range processes {
		deps, root := proc.Dependencies()
		for _, d := range deps {
			if !d.Present() {
				if root {
					r.needsRoot = true
				}
				if !seen[d] {
					seen[d] = true
					r.items = append(r.items, installTask{proc: proc.Name(), dep: d})
				}
			}
		}
	}
	return r
}

// Present reports whether all dependencies are already present.
func (r *Requirements) Present() bool { return len(r.items) == 0 }

// NeedsRoot reports whether any missing dependency requires root privileges.
func (r *Requirements) NeedsRoot() bool { return r.needsRoot }

// Apply attempts to install all missing dependencies.
func (r *Requirements) Apply(host environment.HostActions) error {
	for _, task := range r.items {
		if err := task.dep.Apply(host); err != nil {
			return fmt.Errorf("failed to apply dependency for %s: %w", task.proc, err)
		}
	}
	return nil
}

// Dependencies is the legacy entrypoint that returns a single combined Dependency
// and a root flag. New code should prefer Resolve.
func Dependencies(processes ...Process) (deps Dependency, root bool) {
	r := Resolve(processes...)
	return r, r.NeedsRoot()
}
