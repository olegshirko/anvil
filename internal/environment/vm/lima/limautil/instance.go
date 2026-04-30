package limautil

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"anvil/internal/domain"
)

type cachedInstance struct {
	info      InstanceInfo
	err       error
	timestamp time.Time
}

var (
	instanceCache    = make(map[string]cachedInstance)
	instanceCacheMu  sync.RWMutex
	instanceCacheTTL = 2 * time.Second

	allInstancesCache     []InstanceInfo
	allInstancesCacheErr  error
	allInstancesCacheTime time.Time
)

// Instance returns current instance.
func Instance(profileID string) (InstanceInfo, error) {
	instanceCacheMu.RLock()
	cached, ok := instanceCache[profileID]
	instanceCacheMu.RUnlock()
	if ok && time.Since(cached.timestamp) < instanceCacheTTL {
		return cached.info, cached.err
	}

	info, err := getInstance(profileID)
	instanceCacheMu.Lock()
	instanceCache[profileID] = cachedInstance{info: info, err: err, timestamp: time.Now()}
	instanceCacheMu.Unlock()
	return info, err
}

// InstanceInfo is the information about a Lima instance
type InstanceInfo struct {
	Name    string `json:"name,omitempty"`
	Status  string `json:"status,omitempty"`
	Arch    string `json:"arch,omitempty"`
	CPU     int    `json:"cpus,omitempty"`
	Memory  int64  `json:"memory,omitempty"`
	Disk    int64  `json:"disk,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Network []struct {
		VNL       string `json:"vnl,omitempty"`
		Interface string `json:"interface,omitempty"`
	} `json:"network,omitempty"`
	IPAddress string `json:"address,omitempty"`
	Runtime   string `json:"runtime,omitempty"`
}

// Running checks if the instance is running.
func (i InstanceInfo) Running() bool { return i.Status == limaStatusRunning }

// Config returns the current anvil config
func (i InstanceInfo) Config(stateFile string) (domain.Config, error) {
	return LoadInstance(&domain.Profile{ID: i.Name}, stateFile)
}

// Lima statuses
const (
	limaStatusRunning = "Running"
)

func getInstance(profileID string) (InstanceInfo, error) {
	var i InstanceInfo
	var buf bytes.Buffer
	cmd := Limactl("list", profileID, "--json")
	cmd.Stderr = nil
	cmd.Stdout = &buf

	if err := cmd.Run(); err != nil {
		return i, fmt.Errorf("error retrieving instance: %w", err)
	}

	if buf.Len() == 0 {
		return i, fmt.Errorf("instance '%s' does not exist", profileID)
	}

	if err := json.Unmarshal(buf.Bytes(), &i); err != nil {
		return i, fmt.Errorf("error retrieving instance: %w", err)
	}
	return i, nil
}

// Instances returns Lima instances created by anvil.
// The ids should be Lima instance IDs.
func Instances(ids ...string) ([]InstanceInfo, error) {
	// cache only the "all instances" query (no specific ids)
	if len(ids) == 0 {
		instanceCacheMu.RLock()
		if !allInstancesCacheTime.IsZero() && time.Since(allInstancesCacheTime) < instanceCacheTTL {
			cached := allInstancesCache
			cachedErr := allInstancesCacheErr
			instanceCacheMu.RUnlock()
			return cached, cachedErr
		}
		instanceCacheMu.RUnlock()
	}

	args := append([]string{"list", "--json"}, ids...)

	var buf bytes.Buffer
	cmd := Limactl(args...)
	cmd.Stderr = nil
	cmd.Stdout = &buf

	if err := cmd.Run(); err != nil {
		if len(ids) == 0 {
			instanceCacheMu.Lock()
			allInstancesCache = nil
			allInstancesCacheErr = fmt.Errorf("error retrieving instances: %w", err)
			allInstancesCacheTime = time.Now()
			instanceCacheMu.Unlock()
		}
		return nil, fmt.Errorf("error retrieving instances: %w", err)
	}

	var instances []InstanceInfo
	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		var i InstanceInfo
		line := scanner.Bytes()
		if err := json.Unmarshal(line, &i); err != nil {
			if len(ids) == 0 {
				instanceCacheMu.Lock()
				allInstancesCache = nil
				allInstancesCacheErr = fmt.Errorf("error retrieving instances: %w", err)
				allInstancesCacheTime = time.Now()
				instanceCacheMu.Unlock()
			}
			return nil, fmt.Errorf("error retrieving instances: %w", err)
		}

		// limit to anvil instances
		if !strings.HasPrefix(i.Name, "anvil") {
			continue
		}

		instances = append(instances, i)
	}

	if len(ids) == 0 {
		instanceCacheMu.Lock()
		allInstancesCache = instances
		allInstancesCacheErr = nil
		allInstancesCacheTime = time.Now()
		instanceCacheMu.Unlock()
	}

	return instances, nil
}

// RunningInstances return Lima instances that are has a running status.
func RunningInstances() ([]InstanceInfo, error) {
	allInstances, err := Instances()
	if err != nil {
		return nil, err
	}

	var runningInstances []InstanceInfo
	for _, instance := range allInstances {
		if instance.Running() {
			runningInstances = append(runningInstances, instance)
		}
	}

	return runningInstances, nil
}
