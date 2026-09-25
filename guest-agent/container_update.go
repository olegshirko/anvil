package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// `docker update`: resource limits and the restart policy of an existing
// container. Limits are written to the containerd spec (so the next start
// keeps them), applied to a live task through runc update, and mirrored in
// the metadata snapshot that inspect reports. As in Docker, zero fields
// mean "leave unchanged".

// dockerUpdateRequest is the /containers/{id}/update body: the Resources
// fields at top level plus RestartPolicy. dockerHostConfig already carries
// both under the same JSON names.
type dockerUpdateRequest = dockerHostConfig

func handleContainerUpdate(w http.ResponseWriter, r *http.Request, p routeParams) {
	var req dockerUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid update body: "+err.Error())
		return
	}
	warnings, err := updateDockerContainer(r.Context(), p["id"], req)
	if err != nil {
		status := http.StatusBadRequest
		if _, _, _, rerr := resolveDockerID(r.Context(), p["id"]); rerr != nil {
			status = http.StatusNotFound
		}
		writeJSONError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"Warnings": warnings})
}

func updateDockerContainer(ctx context.Context, ref string, req dockerUpdateRequest) ([]string, error) {
	ns, cid, _, err := resolveDockerID(ctx, ref)
	if err != nil {
		return nil, err
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, cid)
	if err != nil {
		return nil, fmt.Errorf("load container: %w", err)
	}
	meta, _ := loadContainerMeta(ns, cid)
	if req.RestartPolicy.Name != "" && req.RestartPolicy.Name != "no" && meta != nil && meta.AutoRemove {
		return nil, fmt.Errorf("restart policy cannot be updated because AutoRemove is enabled for the container")
	}

	if hasResourceUpdate(req) {
		spec, err := c.Spec(nsCtx)
		if err != nil {
			return nil, fmt.Errorf("container spec: %w", err)
		}
		if spec.Linux == nil {
			spec.Linux = &specs.Linux{}
		}
		if spec.Linux.Resources == nil {
			spec.Linux.Resources = &specs.LinuxResources{}
		}
		if err := applyResourceUpdate(spec.Linux.Resources, req); err != nil {
			return nil, err
		}
		if task, terr := c.Task(nsCtx, nil); terr == nil {
			if st, serr := task.Status(nsCtx); serr == nil && (st.Status == client.Running || st.Status == client.Paused) {
				if err := task.Update(nsCtx, client.WithResources(spec.Linux.Resources)); err != nil {
					return nil, fmt.Errorf("update running container: %w", err)
				}
			}
		}
		if err := c.Update(nsCtx, client.UpdateContainerOpts(client.WithSpec(spec))); err != nil {
			return nil, fmt.Errorf("update container spec: %w", err)
		}
	}

	if req.RestartPolicy.Name != "" {
		p := parseRestartPolicy(req.RestartPolicy.Name)
		if p.max < 0 && req.RestartPolicy.MaximumRetryCount > 0 {
			p.max = req.RestartPolicy.MaximumRetryCount
		}
		running, _, _ := containerTaskState(ctx, ns, cid)
		restarts.update(ns, cid, p.name, p.max, running)
	}

	if meta != nil {
		if meta.HostConfig == nil {
			meta.HostConfig = &dockerHostConfig{}
		}
		mergeHostConfigUpdate(meta.HostConfig, req)
		if err := saveContainerMeta(meta); err != nil {
			return nil, err
		}
	}
	return []string{}, nil
}

func hasResourceUpdate(u dockerUpdateRequest) bool {
	return u.Memory != 0 || u.MemorySwap != 0 || u.MemoryReservation != 0 ||
		u.NanoCpus != 0 || u.CpuShares != 0 || u.CpuPeriod != 0 || u.CpuQuota != 0 ||
		u.CpusetCpus != "" || u.CpusetMems != "" || (u.PidsLimit != nil && *u.PidsLimit != 0)
}

// applyResourceUpdate folds the non-zero update fields into the OCI
// resources, with the same translations the create path uses.
func applyResourceUpdate(res *specs.LinuxResources, u dockerUpdateRequest) error {
	if u.NanoCpus != 0 && (u.CpuPeriod != 0 || u.CpuQuota != 0) {
		return fmt.Errorf("conflicting options: CPU Quota cannot be updated as NanoCPUs has already been set")
	}
	if u.Memory != 0 || u.MemorySwap != 0 || u.MemoryReservation != 0 {
		if res.Memory == nil {
			res.Memory = &specs.LinuxMemory{}
		}
		m := res.Memory
		memory := u.Memory
		if memory == 0 && m.Limit != nil {
			memory = *m.Limit
		}
		if u.Memory != 0 {
			if u.Memory < 6*1024*1024 {
				return fmt.Errorf("minimum memory limit allowed is 6MB")
			}
			limit := u.Memory
			m.Limit = &limit
		}
		if u.MemorySwap != 0 {
			swap, err := memorySwapSpec(memory, u.MemorySwap)
			if err != nil {
				return err
			}
			m.Swap = swap
		} else if u.Memory != 0 && m.Swap != nil && *m.Swap > 0 && *m.Swap < u.Memory {
			return fmt.Errorf("memory limit should be smaller than already set memoryswap limit, update the memoryswap at the same time")
		}
		if u.MemoryReservation != 0 {
			reservation := u.MemoryReservation
			m.Reservation = &reservation
		}
	}
	if u.NanoCpus != 0 || u.CpuShares != 0 || u.CpuPeriod != 0 || u.CpuQuota != 0 || u.CpusetCpus != "" || u.CpusetMems != "" {
		if res.CPU == nil {
			res.CPU = &specs.LinuxCPU{}
		}
		cpu := res.CPU
		if u.NanoCpus != 0 {
			quota := u.NanoCpus * cpuPeriod / 1e9
			period := uint64(cpuPeriod)
			cpu.Quota, cpu.Period = &quota, &period
		}
		if u.CpuPeriod != 0 {
			period := uint64(u.CpuPeriod)
			cpu.Period = &period
		}
		if u.CpuQuota != 0 {
			quota := u.CpuQuota
			cpu.Quota = &quota
		}
		if u.CpuShares != 0 {
			shares := uint64(u.CpuShares)
			cpu.Shares = &shares
		}
		if u.CpusetCpus != "" {
			cpu.Cpus = u.CpusetCpus
		}
		if u.CpusetMems != "" {
			cpu.Mems = u.CpusetMems
		}
	}
	if u.PidsLimit != nil && *u.PidsLimit != 0 {
		limit := *u.PidsLimit
		if limit < 0 {
			limit = 0 // runc: 0 = no limit (pids.max "max")
		}
		res.Pids = &specs.LinuxPids{Limit: &limit}
	}
	return nil
}

// mergeHostConfigUpdate mirrors an update into the HostConfig snapshot
// inspect reports.
func mergeHostConfigUpdate(hc *dockerHostConfig, u dockerUpdateRequest) {
	if u.Memory != 0 {
		hc.Memory = u.Memory
	}
	if u.MemorySwap != 0 {
		hc.MemorySwap = u.MemorySwap
	}
	if u.MemoryReservation != 0 {
		hc.MemoryReservation = u.MemoryReservation
	}
	if u.NanoCpus != 0 {
		hc.NanoCpus = u.NanoCpus
	}
	if u.CpuShares != 0 {
		hc.CpuShares = u.CpuShares
	}
	if u.CpuPeriod != 0 {
		hc.CpuPeriod = u.CpuPeriod
	}
	if u.CpuQuota != 0 {
		hc.CpuQuota = u.CpuQuota
	}
	if u.CpusetCpus != "" {
		hc.CpusetCpus = u.CpusetCpus
	}
	if u.CpusetMems != "" {
		hc.CpusetMems = u.CpusetMems
	}
	if u.PidsLimit != nil && *u.PidsLimit != 0 {
		hc.PidsLimit = u.PidsLimit
	}
	if u.RestartPolicy.Name != "" {
		hc.RestartPolicy = u.RestartPolicy
	}
}
