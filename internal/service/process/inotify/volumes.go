package inotify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"anvil/internal/environment/container/containerd"
	"anvil/internal/environment/container/docker"
)

// trackContainerMounts starts a background goroutine that periodically
// discovers container volumes and sends the updated list to the supplied channel.
func (w *fsWatcher) trackContainerMounts(ctx context.Context, out chan<- []string) error {
	if w.runtime == "" {
		return fmt.Errorf("container runtime not set")
	}

	go w.pollLoop(ctx, out)
	return nil
}

// pollLoop polls the runtime for mounts at fixed intervals until ctx is cancelled.
func (w *fsWatcher) pollLoop(ctx context.Context, out chan<- []string) {
	log := w.log
	tick := time.NewTicker(volumesInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Trace("mount tracker shutting down")
			if err := ctx.Err(); err != nil {
				log.Tracef("mount tracker stop reason: %v", err)
			}
			return
		case <-tick.C:
			mnts, err := w.pollMounts()
			if err != nil {
				log.Error(err)
				continue
			}
			out <- mnts
		}
	}
}

// pollMounts retrieves the current set of mounted directories from the active
// container runtime (docker or containerd/nerdctl).
func (w *fsWatcher) pollMounts() ([]string, error) {
	switch w.runtime {
	case docker.Name:
		return w.collectRuntimeMounts(docker.Name)
	case containerd.Name:
		return w.pollContainerdMounts()
	default:
		return nil, nil
	}
}

// pollContainerdMounts aggregates mounts across all containerd namespaces.
func (w *fsWatcher) pollContainerdMounts() ([]string, error) {
	raw, err := w.guest.RunOutput("sudo", "nerdctl", "namespace", "list", "-q")
	if err != nil {
		return nil, fmt.Errorf("cannot list containerd namespaces: %w", err)
	}
	nsList := strings.Fields(raw)

	var all []string
	for _, ns := range nsList {
		mnts, err := w.collectRuntimeMounts("sudo", "nerdctl", "--namespace", ns)
		if err != nil {
			return nil, fmt.Errorf("cannot list mounts for namespace %q: %w", ns, err)
		}
		all = append(all, mnts...)
	}
	return all, nil
}

// collectRuntimeMounts returns bound host directories for the given containers.
func (w *fsWatcher) collectRuntimeMounts(cmdArgs ...string) ([]string, error) {
	log := w.log

	ids, err := w.listContainerIDs(cmdArgs...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	log.Tracef("found containers: %+v", ids)

	inspections, err := w.inspectContainers(cmdArgs, ids)
	if err != nil {
		return nil, err
	}

	var mounts []string
	for _, in := range inspections {
		for _, m := range in.Mounts {
			if w.underWatchRoot(m.Source) {
				mounts = append(mounts, m.Source)
			}
		}
	}

	pruned := pruneSubpaths(mounts)
	log.Tracef("active mounts after dedup: %+v", pruned)
	return pruned, nil
}

// listContainerIDs returns the IDs of running containers for the given runtime command.
func (w *fsWatcher) listContainerIDs(cmdArgs ...string) ([]string, error) {
	args := make([]string, 0, len(cmdArgs)+2)
	args = append(args, cmdArgs...)
	args = append(args, "ps", "-q")
	out, err := w.guest.RunOutput(args...)
	if err != nil {
		return nil, fmt.Errorf("cannot list containers: %w", err)
	}
	return strings.Fields(out), nil
}

// inspectContainers runs `inspect` against the provided IDs and parses the JSON output.
func (w *fsWatcher) inspectContainers(cmdArgs, ids []string) ([]containerInspect, error) {
	args := make([]string, 0, len(cmdArgs)+1+len(ids))
	args = append(args, cmdArgs...)
	args = append(args, "inspect")
	args = append(args, ids...)

	var buf bytes.Buffer
	if err := w.guest.RunWith(nil, &buf, args...); err != nil {
		return nil, fmt.Errorf("cannot inspect containers: %w", err)
	}

	var result []containerInspect
	if err := json.NewDecoder(&buf).Decode(&result); err != nil {
		return nil, fmt.Errorf("cannot decode container inspect JSON: %w", err)
	}
	return result, nil
}

// containerInspect is the minimal shape we need from `docker/nerdctl inspect`.
type containerInspect struct {
	Mounts []struct {
		Source string `json:"Source"`
	} `json:"Mounts"`
}

// underWatchRoot reports whether the path is inside a directory that the VM exposes.
func (w *fsWatcher) underWatchRoot(child string) bool {
	for _, parent := range w.vmVols {
		if strings.HasPrefix(child, parent) {
			return true
		}
	}
	return false
}

// pruneSubpaths removes directories that are children of another directory in
// the same slice, keeping only the top-most paths.
func pruneSubpaths(paths []string) []string {
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil
	}

	unique := make([]string, 0, len(paths))
	unique = append(unique, paths[0])

	for _, p := range paths[1:] {
		last := unique[len(unique)-1]
		if p == last {
			continue // duplicate
		}
		if strings.HasPrefix(p, strings.TrimSuffix(last, "/")+"/") {
			continue // p is inside last
		}
		unique = append(unique, p)
	}
	return unique
}
