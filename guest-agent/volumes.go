package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
)

// dockerVolume matches the JSON returned by GET /volumes and /volumes/{name}.
type dockerVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
	Scope      string            `json:"Scope"`
}

// dockerVolumeList is the response for GET /volumes.
type dockerVolumeList struct {
	Volumes  []dockerVolume `json:"Volumes"`
	Warnings []string       `json:"Warnings"`
}

// nerdctlVolumeLs is the shape of `nerdctl volume ls --format json` output.
type nerdctlVolumeLs struct {
	Name       string `json:"Name"`
	Driver     string `json:"Driver"`
	Mountpoint string `json:"Mountpoint"`
	Labels     string `json:"Labels"`
	Scope      string `json:"Scope"`
	Size       string `json:"Size"`
}

// nerdctlVolumeInspect is the shape of `nerdctl volume inspect --format json` output.
type nerdctlVolumeInspect struct {
	Name       string            `json:"Name"`
	Mountpoint string            `json:"Mountpoint"`
	Labels     map[string]string `json:"Labels"`
}

// dockerVolumeCreateRequest mirrors Docker's POST /volumes/create body.
type dockerVolumeCreateRequest struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Options map[string]string `json:"DriverOpts"`
	Labels  map[string]string `json:"Labels"`
}

// listDockerVolumes returns volumes from all namespaces.
func listDockerVolumes(filters map[string]map[string]bool) ([]dockerVolume, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	hasDefault := false
	for _, ns := range nss {
		if ns == "default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		nss = append([]string{"default"}, nss...)
	}

	result := make([]dockerVolume, 0)
	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "volume", "ls", "--format", "json")
		if err != nil || code != 0 {
			log.Printf("[docker-api] volume ls in %s: %d %s%s", ns, code, stdout, stderr)
			continue
		}

		for _, line := range strings.Split(stdout, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var vol nerdctlVolumeLs
			if err := json.Unmarshal([]byte(line), &vol); err != nil {
				continue
			}
			labels := parseNerdctlLabels(vol.Labels)
			dv := dockerVolume{
				Name:       vol.Name,
				Driver:     defaultString(vol.Driver, "local"),
				Mountpoint: vol.Mountpoint,
				CreatedAt:  time.Now().UTC().Format(time.RFC3339),
				Labels:     labels,
				Options:    map[string]string{},
				Scope:      defaultString(vol.Scope, "local"),
			}
			if matchesLabelFilters(labels, filters) {
				result = append(result, dv)
			}
		}
	}
	return result, nil
}

// inspectDockerVolume returns a volume by name from any namespace.
func inspectDockerVolume(name string) (*dockerVolume, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	hasDefault := false
	for _, ns := range nss {
		if ns == "default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		nss = append([]string{"default"}, nss...)
	}

	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "volume", "inspect", "--format", "json", name)
		if err != nil || code != 0 {
			log.Printf("[docker-api] volume inspect in %s: %d %s%s", ns, code, stdout, stderr)
			continue
		}
		for _, line := range strings.Split(stdout, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var vol nerdctlVolumeInspect
			if err := json.Unmarshal([]byte(line), &vol); err != nil {
				continue
			}
			if vol.Name == name {
				labels := vol.Labels
				if labels == nil {
					labels = map[string]string{}
				}
				dv := dockerVolume{
					Name:       vol.Name,
					Driver:     "local",
					Mountpoint: vol.Mountpoint,
					CreatedAt:  time.Now().UTC().Format(time.RFC3339),
					Labels:     labels,
					Options:    map[string]string{},
					Scope:      "local",
				}
				return &dv, nil
			}
		}
	}
	return nil, fmt.Errorf("No such volume: %s", name)
}

// createDockerVolume creates a volume in the default namespace.
// nerdctl volume create only supports --label, so Driver/Options are ignored.
func createDockerVolume(req dockerVolumeCreateRequest) (*dockerVolume, error) {
	ns := "default"
	args := []string{"volume", "create"}
	for k, v := range req.Labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, req.Name)

	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 || strings.Contains(stderr, "fatal") {
		return nil, fmt.Errorf("nerdctl volume create failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	volName := strings.TrimSpace(stdout)
	if volName == "" {
		volName = req.Name
	}
	return inspectDockerVolume(volName)
}

// removeDockerVolume removes a volume by name from any namespace.
func removeDockerVolume(name string) error {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	hasDefault := false
	for _, ns := range nss {
		if ns == "default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		nss = append([]string{"default"}, nss...)
	}

	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "volume", "rm", name)
		if err == nil && code == 0 {
			return nil
		}
		_ = stdout
		_ = stderr
	}
	return fmt.Errorf("No such volume: %s", name)
}
