package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// handleForcedMirrorPull serves `docker pull mirror.anvil/<image>`: it loads
// straight from the docker-mirror GitHub releases — Docker Hub is never
// contacted, so this works on hosts whose registry route is dead or
// rate-limited. The image lands under its real name (docker run
// postgres:15.5 works) and under the mirror.anvil/… ref it was pulled by
// (compose files can pin the forced source).
func handleForcedMirrorPull(w http.ResponseWriter, r *http.Request, rawRef, realRef string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	prog := newPullProgress(w)
	prog.send(map[string]any{"status": "Pulling from docker-mirror", "id": realRef})

	platform, err := resolveRequestedPlatform(r.URL.Query().Get("platform"))
	if err == nil {
		err = loadFromMirrorPlatform(r.Context(), realRef, "default", platform)
	}
	if err != nil {
		msg := err.Error()
		if errors.Is(err, errMirrorNotFound) {
			msg = fmt.Sprintf("image %q is not mirrored in %s — publish it with: anvil images push %s",
				realRef, dockerMirrorRepo, realRef)
		}
		prog.send(map[string]any{
			"error":       fmt.Sprintf("error pulling %s: %s", rawRef, msg),
			"errorDetail": map[string]string{"message": msg},
		})
		return
	}

	// Alias under the mirror.anvil ref it was pulled by. The import inside
	// loadFromMirrorPlatform already tagged the canonical real name.
	canonical := canonicalizeImageRef(realRef)
	if cl, cerr := pc.get(r.Context()); cerr == nil {
		nsCtx := namespaces.WithNamespace(r.Context(), "default")
		if img, ierr := cl.GetImage(nsCtx, canonical); ierr == nil && rawRef != canonical {
			if aerr := putImage(cl, nsCtx, images.Image{Name: rawRef, Target: img.Target(), Labels: img.Labels()}); aerr != nil {
				prog.status("mirror alias " + rawRef + ": " + aerr.Error())
			}
		}
	}
	publishObjectEvent("image", "pull", canonical, map[string]string{"name": rawRef})
	if d := localImageDigest(r.Context(), "default", realRef); d != "" {
		prog.status("Digest: " + d)
	}
	prog.status("Status: Downloaded newer image for " + rawRef)
	prog.complete()
}

// mirrorForcedHost is the registry host that forces a docker-mirror load:
// `docker pull mirror.anvil/<image>` loads straight from the docker-mirror
// GitHub releases and never contacts Docker Hub — for hosts without a
// working registry route, or before the rate limit resets. The loaded image
// is tagged with its real name too, so `docker run <image>` works as usual,
// and with the mirror.anvil/… alias so compose files can pin the forced
// source explicitly.
const mirrorForcedHost = "mirror.anvil"

// splitMirrorForcedRef recognizes a forced-mirror pull ref and returns the
// real image ref behind it. Anything else (ok=false) goes down the normal
// pull path untouched.
func splitMirrorForcedRef(ref string) (string, bool) {
	host, rest, found := strings.Cut(ref, "/")
	if !found || !strings.EqualFold(host, mirrorForcedHost) || rest == "" {
		return "", false
	}
	return rest, true
}

// mirrorReleaseAsset maps a real image ref plus requested platform to the
// docker-mirror release tag and asset name. arm64 is the mirror's native
// platform (release `<safe-name>-<tag>-arm64`); amd64 images publish a
// separate `-amd64` release carrying image-amd64.tar.zst. Other platforms
// are not mirrored.
func mirrorReleaseAsset(ref, platform string) (release, asset string, err error) {
	name, tag, ok := splitImageTag(ref)
	if !ok {
		return "", "", errMirrorNotFound
	}
	safe := strings.ReplaceAll(name, "/", "-")
	switch platform {
	case "", "linux/arm64":
		return safe + "-" + tag + "-arm64", "image-arm64.tar.zst", nil
	case "linux/amd64":
		return safe + "-" + tag + "-amd64", "image-amd64.tar.zst", nil
	}
	return "", "", fmt.Errorf("platform %q is not mirrored: only linux/arm64 and linux/amd64 releases exist", platform)
}
