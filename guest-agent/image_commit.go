package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/rootfs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// `docker commit`: the container's rootfs changes become one new gzip
// layer on top of its image, with the container's process config (cmd,
// entrypoint, env, workdir) as the new image config — Docker semantics.
// Body config fields and `--change` Dockerfile instructions override it.

// dockerCommitConfig is the subset of a container Config the commit body
// may override.
type dockerCommitConfig struct {
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
	Env          []string            `json:"Env"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	Labels       map[string]string   `json:"Labels"`
	User         string              `json:"User"`
	WorkingDir   string              `json:"WorkingDir"`
	Volumes      map[string]struct{} `json:"Volumes"`
	StopSignal   string              `json:"StopSignal"`
}

type commitOptions struct {
	container, repo, tag, comment, author string
	pause                                 bool
	changes                               []string
	config                                *dockerCommitConfig
}

func handleCommit(w http.ResponseWriter, r *http.Request, _ routeParams) {
	q := r.URL.Query()
	opts := commitOptions{
		container: q.Get("container"),
		repo:      q.Get("repo"),
		tag:       q.Get("tag"),
		comment:   q.Get("comment"),
		author:    q.Get("author"),
		pause:     q.Get("pause") != "0" && q.Get("pause") != "false",
		changes:   q["changes"],
	}
	if opts.container == "" {
		writeJSONError(w, http.StatusBadRequest, "container is required")
		return
	}
	var body dockerCommitConfig
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
		opts.config = &body
	}
	if _, _, _, err := resolveDockerID(r.Context(), opts.container); err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	id, err := commitContainer(r.Context(), opts)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"Id": id})
}

func commitContainer(ctx context.Context, o commitOptions) (string, error) {
	ns, cid, _, err := resolveDockerID(ctx, o.container)
	if err != nil {
		return "", err
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return "", fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	// A lease keeps the new blobs alive until the image record refers to them.
	nsCtx, done, err := cl.WithLease(nsCtx)
	if err != nil {
		return "", fmt.Errorf("lease: %w", err)
	}
	defer done(context.WithoutCancel(nsCtx))

	c, err := cl.LoadContainer(nsCtx, cid)
	if err != nil {
		return "", fmt.Errorf("load container: %w", err)
	}
	info, err := c.Info(nsCtx)
	if err != nil {
		return "", fmt.Errorf("container info: %w", err)
	}
	if info.SnapshotKey == "" {
		return "", fmt.Errorf("container has no rootfs snapshot")
	}
	baseImg, err := cl.GetImage(nsCtx, info.Image)
	if err != nil {
		return "", fmt.Errorf("base image %s: %w", info.Image, err)
	}
	cs := cl.ContentStore()
	baseManifest, err := images.Manifest(nsCtx, cs, baseImg.Target(), baseImg.Platform())
	if err != nil {
		return "", fmt.Errorf("base manifest: %w", err)
	}
	var baseConfig ocispec.Image
	if blob, err := content.ReadBlob(nsCtx, cs, baseManifest.Config); err != nil {
		return "", fmt.Errorf("base config: %w", err)
	} else if err := json.Unmarshal(blob, &baseConfig); err != nil {
		return "", fmt.Errorf("base config: %w", err)
	}

	if o.pause {
		if task, terr := c.Task(nsCtx, nil); terr == nil {
			if st, serr := task.Status(nsCtx); serr == nil && st.Status == client.Running {
				if err := task.Pause(nsCtx); err == nil {
					defer task.Resume(context.WithoutCancel(nsCtx)) //nolint:errcheck
				}
			}
		}
	}

	dockerTypes := baseManifest.MediaType == images.MediaTypeDockerSchema2Manifest ||
		baseManifest.Config.MediaType == images.MediaTypeDockerSchema2Config
	layerType, configType, manifestType := ocispec.MediaTypeImageLayerGzip, ocispec.MediaTypeImageConfig, ocispec.MediaTypeImageManifest
	if dockerTypes {
		layerType, configType, manifestType = images.MediaTypeDockerSchema2LayerGzip, images.MediaTypeDockerSchema2Config, images.MediaTypeDockerSchema2Manifest
	}

	layer, err := rootfs.CreateDiff(nsCtx, info.SnapshotKey, cl.SnapshotService(info.Snapshotter), cl.DiffService(),
		diff.WithMediaType(layerType), diff.WithReference("anvil-commit-"+cid))
	if err != nil {
		return "", fmt.Errorf("diff rootfs: %w", err)
	}
	layerInfo, err := cs.Info(nsCtx, layer.Digest)
	if err != nil {
		return "", fmt.Errorf("layer info: %w", err)
	}
	diffID, err := digest.Parse(layerInfo.Labels["containerd.io/uncompressed"])
	if err != nil {
		return "", fmt.Errorf("layer has no uncompressed digest: %w", err)
	}

	cfg := baseConfig
	cfg.Config = committedProcessConfig(nsCtx, c, cid, ns, baseConfig.Config)
	if o.config != nil {
		mergeCommitConfig(&cfg.Config, o.config)
	}
	for _, ch := range o.changes {
		if err := applyCommitChange(&cfg.Config, ch); err != nil {
			return "", err
		}
	}
	now := time.Now().UTC()
	cfg.Created = &now
	if o.author != "" {
		cfg.Author = o.author
	}
	cfg.RootFS.DiffIDs = append(slices.Clone(baseConfig.RootFS.DiffIDs), diffID)
	cfg.History = append(slices.Clone(baseConfig.History), ocispec.History{
		Created: &now, Author: o.author, Comment: o.comment,
	})

	configDesc, err := writeJSONBlob(nsCtx, cs, configType, cfg, nil)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	layers := append(slices.Clone(baseManifest.Layers), layer)
	manifest := struct {
		SchemaVersion int                  `json:"schemaVersion"`
		MediaType     string               `json:"mediaType"`
		Config        ocispec.Descriptor   `json:"config"`
		Layers        []ocispec.Descriptor `json:"layers"`
	}{2, manifestType, configDesc, layers}
	gcLabels := map[string]string{"containerd.io/gc.ref.content.config": configDesc.Digest.String()}
	for i, l := range layers {
		gcLabels["containerd.io/gc.ref.content.l."+strconv.Itoa(i)] = l.Digest.String()
	}
	manifestDesc, err := writeJSONBlob(nsCtx, cs, manifestType, manifest, gcLabels)
	if err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}

	name := commitImageName(o.repo, o.tag, manifestDesc.Digest)
	rec := images.Image{Name: name, Target: manifestDesc, CreatedAt: now, UpdatedAt: now}
	if err := putImage(cl, nsCtx, rec); err != nil {
		return "", fmt.Errorf("create image %s: %w", name, err)
	}
	if err := client.NewImage(cl, rec).Unpack(nsCtx, ""); err != nil {
		return "", fmt.Errorf("unpack %s: %w", name, err)
	}
	return manifestDesc.Digest.String(), nil
}

// commitImageName is the record name: repo[:tag] canonicalized, or a
// digest-only name for an untagged commit (listed as <none>, dangling).
func commitImageName(repo, tag string, dgst digest.Digest) string {
	if repo == "" {
		return "<none>@" + dgst.String()
	}
	if tag != "" {
		repo += ":" + tag
	}
	return canonicalizeImageRef(repo)
}

// committedProcessConfig starts from the base image config and replaces
// what the container actually ran with: args split back into entrypoint and
// cmd, the merged env and the working directory.
func committedProcessConfig(ctx context.Context, c client.Container, cid, ns string, base ocispec.ImageConfig) ocispec.ImageConfig {
	out := base
	spec, err := c.Spec(ctx)
	if err != nil || spec.Process == nil {
		return out
	}
	entrypoint := base.Entrypoint
	if meta, merr := loadContainerMeta(ns, cid); merr == nil && len(meta.Entrypoint) > 0 {
		entrypoint = meta.Entrypoint
	}
	out.Entrypoint, out.Cmd = splitEntrypoint(userProcessArgs(spec.Process.Args), entrypoint)
	out.Env = spec.Process.Env
	if spec.Process.Cwd != "" {
		out.WorkingDir = spec.Process.Cwd
	}
	return out
}

// splitEntrypoint splits argv into (entrypoint, cmd) when argv starts with
// the entrypoint; otherwise the whole argv is the cmd.
func splitEntrypoint(argv, entrypoint []string) ([]string, []string) {
	if len(entrypoint) > 0 && len(argv) >= len(entrypoint) && slices.Equal(argv[:len(entrypoint)], entrypoint) {
		return entrypoint, argv[len(entrypoint):]
	}
	return nil, argv
}

// mergeCommitConfig applies the non-empty fields of the request body.
func mergeCommitConfig(dst *ocispec.ImageConfig, o *dockerCommitConfig) {
	if len(o.Cmd) > 0 {
		dst.Cmd = o.Cmd
	}
	if len(o.Entrypoint) > 0 {
		dst.Entrypoint = o.Entrypoint
	}
	if len(o.Env) > 0 {
		dst.Env = mergeEnv(dst.Env, o.Env)
	}
	for p := range o.ExposedPorts {
		if dst.ExposedPorts == nil {
			dst.ExposedPorts = map[string]struct{}{}
		}
		dst.ExposedPorts[p] = struct{}{}
	}
	for k, v := range o.Labels {
		if dst.Labels == nil {
			dst.Labels = map[string]string{}
		}
		dst.Labels[k] = v
	}
	if o.User != "" {
		dst.User = o.User
	}
	if o.WorkingDir != "" {
		dst.WorkingDir = o.WorkingDir
	}
	for v := range o.Volumes {
		if dst.Volumes == nil {
			dst.Volumes = map[string]struct{}{}
		}
		dst.Volumes[v] = struct{}{}
	}
	if o.StopSignal != "" {
		dst.StopSignal = o.StopSignal
	}
}

// applyCommitChange applies one `--change` Dockerfile instruction. Docker
// accepts CMD, ENTRYPOINT, ENV, EXPOSE, LABEL, ONBUILD, USER, VOLUME,
// WORKDIR and STOPSIGNAL here; ONBUILD is not representable in an OCI
// config and is refused.
func applyCommitChange(cfg *ocispec.ImageConfig, change string) error {
	change = strings.TrimSpace(change)
	instr, rest, _ := strings.Cut(change, " ")
	rest = strings.TrimSpace(rest)
	switch strings.ToUpper(instr) {
	case "CMD":
		cfg.Cmd = parseExecOrShell(rest)
	case "ENTRYPOINT":
		cfg.Entrypoint = parseExecOrShell(rest)
	case "ENV":
		kvs, err := parseKeyValues(rest, true)
		if err != nil {
			return fmt.Errorf("--change %q: %w", change, err)
		}
		cfg.Env = mergeEnv(cfg.Env, kvs)
	case "LABEL":
		kvs, err := parseKeyValues(rest, false)
		if err != nil {
			return fmt.Errorf("--change %q: %w", change, err)
		}
		if cfg.Labels == nil {
			cfg.Labels = map[string]string{}
		}
		for _, kv := range kvs {
			k, v, _ := strings.Cut(kv, "=")
			cfg.Labels[k] = v
		}
	case "EXPOSE":
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = map[string]struct{}{}
		}
		for _, p := range strings.Fields(rest) {
			if !strings.Contains(p, "/") {
				p += "/tcp"
			}
			cfg.ExposedPorts[p] = struct{}{}
		}
	case "USER":
		cfg.User = rest
	case "WORKDIR":
		cfg.WorkingDir = rest
	case "STOPSIGNAL":
		cfg.StopSignal = rest
	case "VOLUME":
		if cfg.Volumes == nil {
			cfg.Volumes = map[string]struct{}{}
		}
		vols := parseExecOrShell(rest)
		if !strings.HasPrefix(rest, "[") {
			vols = strings.Fields(rest)
		}
		for _, v := range vols {
			cfg.Volumes[v] = struct{}{}
		}
	default:
		return fmt.Errorf("--change %q: %s is not a valid change command", change, instr)
	}
	return nil
}

// parseExecOrShell reads a JSON array (exec form) or wraps the string in
// /bin/sh -c (shell form).
func parseExecOrShell(s string) []string {
	if strings.HasPrefix(s, "[") {
		var argv []string
		if json.Unmarshal([]byte(s), &argv) == nil {
			return argv
		}
	}
	return []string{"/bin/sh", "-c", s}
}

// parseKeyValues parses `k=v k2="v 2"` or, for ENV, the legacy `k v` form.
func parseKeyValues(s string, legacySpace bool) ([]string, error) {
	fields, err := splitQuoted(s)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("missing arguments")
	}
	if !strings.Contains(fields[0], "=") {
		if !legacySpace || len(fields) < 2 {
			return nil, fmt.Errorf("expected key=value")
		}
		k, v, _ := strings.Cut(s, " ")
		return []string{k + "=" + strings.TrimSpace(v)}, nil
	}
	for _, f := range fields {
		if !strings.Contains(f, "=") {
			return nil, fmt.Errorf("expected key=value, got %q", f)
		}
	}
	return fields, nil
}

// splitQuoted splits on whitespace, honoring double quotes and backslash
// escapes inside them.
func splitQuoted(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && inQuote && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case ch == '"':
			inQuote, have = !inQuote, true
		case (ch == ' ' || ch == '\t') && !inQuote:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(ch)
			have = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote")
	}
	if have {
		out = append(out, cur.String())
	}
	return out, nil
}

// writeJSONBlob stores v in the content store and returns its descriptor.
func writeJSONBlob(ctx context.Context, cs content.Store, mediaType string, v any, labels map[string]string) (ocispec.Descriptor, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	var opts []content.Opt
	if len(labels) > 0 {
		opts = append(opts, content.WithLabels(labels))
	}
	if err := content.WriteBlob(ctx, cs, "anvil-commit-"+desc.Digest.Encoded(), bytes.NewReader(data), desc, opts...); err != nil {
		return ocispec.Descriptor{}, err
	}
	return desc, nil
}
