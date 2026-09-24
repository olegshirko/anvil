package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// Resolving a user reference (full ID, name, ID prefix) to a containerd
// container, and small per-container task lookups.

// resolveDockerID maps a Docker ID prefix or container name to a containerd ID
// and its namespace. It returns an error if no unique match is found.
func resolveDockerID(ctx context.Context, prefix string) (ns, containerdID, name string, err error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("containerd client: %w", err)
	}

	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("list namespaces: %w", err)
	}

	var cands []containerRef
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			continue
		}
		for _, c := range containers {
			info, err := c.Info(nsCtx)
			if err != nil {
				continue
			}
			labels := info.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			cname := labels[labelName]
			if cname == "" {
				cname = c.ID()
			}

			cands = append(cands, containerRef{ns: ns, id: c.ID(), name: cname})
		}
	}

	m, err := pickContainerRef(prefix, cands)
	if err != nil {
		return "", "", "", err
	}
	return m.ns, m.id, m.name, nil
}

// containerRef is one container as seen by name/ID resolution.
type containerRef struct {
	ns, id, name string
}

// pickContainerRef resolves a user reference in Docker's order: full ID,
// then exact name, then a unique ID prefix. Mixing the three in one pass let
// a name made of hex characters ("db", "cafe") collide with an unrelated
// container whose ID starts that way.
func pickContainerRef(ref string, cands []containerRef) (containerRef, error) {
	name := strings.TrimPrefix(ref, "/")
	if ref == "" || name == "" {
		return containerRef{}, fmt.Errorf("No such container: %s", ref)
	}
	pick := func(match func(containerRef) bool) ([]containerRef, bool) {
		var out []containerRef
		for _, c := range cands {
			if match(c) {
				out = append(out, c)
			}
		}
		return out, len(out) > 0
	}
	if m, ok := pick(func(c containerRef) bool { return dockerID(c.ns, c.id) == ref }); ok {
		return m[0], nil
	}
	if m, ok := pick(func(c containerRef) bool { return c.name == name }); ok {
		if len(m) > 1 {
			return containerRef{}, fmt.Errorf("multiple containers match %s", ref)
		}
		return m[0], nil
	}
	if m, ok := pick(func(c containerRef) bool { return strings.HasPrefix(dockerID(c.ns, c.id), ref) }); ok {
		if len(m) > 1 {
			return containerRef{}, fmt.Errorf("multiple containers match %s", ref)
		}
		return m[0], nil
	}
	return containerRef{}, fmt.Errorf("No such container: %s", ref)
}

// containerTaskState reads the task state for a container directly from
// containerd. Returns ("", false) when the task does not exist.
func containerTaskState(ctx context.Context, ns, id string) (running bool, status string, ok bool) {
	cl, err := pc.get(ctx)
	if err != nil {
		return false, "", false
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return false, "", false
	}
	task, err := c.Task(nsCtx, nil)
	if err != nil {
		// No task: the container exists but was never started (or its last
		// run's task is gone) — Docker reports "created"/"exited" here.
		return false, "created", true
	}
	st, err := task.Status(nsCtx)
	if err != nil {
		return false, "", false
	}
	return st.Status == "running", dockerStatus(string(st.Status)), true
}

// findContainerByName returns the Docker ID of a container with the given name
// in the given namespace, or an empty string if none exists.
func findContainerByName(ctx context.Context, ns, name string) (string, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return "", err
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	containers, err := cl.Containers(nsCtx)
	if err != nil {
		return "", err
	}
	for _, c := range containers {
		labels, err := c.Labels(nsCtx)
		if err != nil {
			continue
		}
		if labels[labelName] == name {
			return dockerID(ns, c.ID()), nil
		}
	}
	return "", nil
}
