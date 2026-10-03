#!/bin/bash
# Build and publish a Homebrew bottle for an already-published GitHub release.
#
# Usage: scripts/make_bottle.sh <version> [tap-dir]
#
# Flow: reinstall the formula from the tap with --build-bottle, run
# `brew bottle --no-rebuild` (rebuild must stay 0 — update-brew pushes the
# cleaned formula first, so brew's origin/HEAD comparison would bump it,
# and nonzero rebuild breaks zerobrew's URL scheme), upload the tarball to
# the GitHub release (brew fetches bottles with a single dash between name
# and version, while `brew bottle` writes a double dash — the asset is
# renamed), upload the same tarball under the other supported macOS tags,
# insert/replace the bottle block in the formula, commit and push the tap.
#
# Uploads are retried (GitHub's upload endpoint answers a transient 400/5xx
# now and then). --resume skips the build when the release already carries
# the primary bottle — after a run that failed past that upload — and
# finishes the aliases and the formula from it.
set -euo pipefail

RESUME=0
if [ "${1:-}" = "--resume" ]; then RESUME=1; shift; fi
VERSION="${1:?usage: make_bottle.sh [--resume] <version> [tap-dir]}"
TAP_DIR="${2:-$(cd "$(dirname "$0")/../../homebrew-tap" && pwd)}"
FORMULA="$TAP_DIR/anvil.rb"
FORMULA_REF="olegshirko/tap/anvil"
REPO="olegshirko/anvil"

test -f "$FORMULA" || { echo "[bottle] error: $FORMULA not found"; exit 1; }
command -v gh >/dev/null || { echo "[bottle] error: gh CLI required"; exit 1; }

# upload FILE: gh release upload with retries and backoff.
upload() {
  local attempt
  for attempt in 1 2 3 4 5; do
    gh release upload "v$VERSION" "$1" --repo "$REPO" --clobber && return 0
    echo "[bottle] upload of $(basename "$1") failed (attempt $attempt), retrying..."
    sleep $((attempt * 5))
  done
  echo "[bottle] error: could not upload $(basename "$1"); rerun with --resume"
  return 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

if [ "$RESUME" = 1 ]; then
  # The primary bottle is the one for this Mac's tag; take it from the release.
  TAG="$(brew ruby -e 'puts Utils::Bottles.tag' 2>/dev/null)"
  PUBLISH_NAME="anvil-$VERSION.$TAG.bottle.tar.gz"
  echo "[bottle] resuming from the published $PUBLISH_NAME..."
  gh release download "v$VERSION" --repo "$REPO" -p "$PUBLISH_NAME" -D "$WORK" \
    || { echo "[bottle] error: $PUBLISH_NAME is not on release v$VERSION; run without --resume"; exit 1; }
  BUILT="$WORK/$PUBLISH_NAME"
  SHA256="$(shasum -a 256 "$BUILT" | cut -d' ' -f1)"
  REBUILD=0
  echo "[bottle] tag=$TAG rebuild=$REBUILD sha256=$SHA256"
else
  echo "[bottle] refreshing taps (a stale tap cache bottles the OLD formula"
  echo "[bottle] and brew bottle then bumps rebuild instead of starting at 0)..."
  brew update >/dev/null

  echo "[bottle] reinstalling $FORMULA_REF with --build-bottle..."
  brew uninstall "$FORMULA_REF" 2>/dev/null || true
  brew install --build-bottle "$FORMULA_REF"

  echo "[bottle] running brew bottle..."
  # --no-rebuild: brew bottle otherwise sets rebuild = upstream(origin/HEAD)
  # rebuild + 1, and because update-brew pushes the cleaned formula before
  # bottling, upstream matches and every release got rebuild 1. Nonzero
  # rebuild breaks zerobrew, which uses its own incompatible URL scheme.
  ( cd "$WORK" && brew bottle --json --no-rebuild "$FORMULA_REF" > bottle-out.txt )

  BUILT="$(ls "$WORK"/anvil--*.bottle*.tar.gz | head -1)"
  # anvil--1.0.37.arm64_tahoe.bottle.1.tar.gz -> anvil-1.0.37.arm64_tahoe.bottle.1.tar.gz
  PUBLISH_NAME="$(basename "$BUILT" | sed 's/^anvil--/anvil-/')"
  TAG="$(basename "$BUILT" | sed -E 's/^anvil--[0-9.]+\.([a-z0-9_]+)\.bottle.*$/\1/')"
  SHA256="$(shasum -a 256 "$BUILT" | cut -d' ' -f1)"
  REBUILD="$(grep -oE '^    rebuild [0-9]+' "$WORK/bottle-out.txt" | awk '{print $2}' || true)"
  REBUILD="${REBUILD:-0}"
  echo "[bottle] tag=$TAG rebuild=$REBUILD sha256=$SHA256"

  echo "[bottle] uploading $PUBLISH_NAME to release v$VERSION..."
  cp "$BUILT" "$WORK/$PUBLISH_NAME"
  upload "$WORK/$PUBLISH_NAME"
  # Drop a stale double-dash asset from a previous manual upload, if any.
  gh release delete-asset "v$VERSION" "$(basename "$BUILT")" --repo "$REPO" --yes 2>/dev/null || true
  # Clients that ignore the rebuild field (e.g. zerobrew) build the bottle URL
  # without the rebuild suffix; publish a rebuild-less alias of the same tarball.
  if [ "$REBUILD" != "0" ]; then
    ALIAS_NAME="$(echo "$PUBLISH_NAME" | sed -E 's/\.bottle\.[0-9]+\.tar\.gz$/.bottle.tar.gz/')"
    echo "[bottle] uploading rebuild-less alias $ALIAS_NAME..."
    cp "$BUILT" "$WORK/$ALIAS_NAME"
    upload "$WORK/$ALIAS_NAME"
  fi
fi

# The bottle is cellar :any_skip_relocation and contains no per-OS build
# artifacts, so the same tarball serves every supported macOS tag. Publish
# it under the other tags too — older macOS releases (and zerobrew, which
# has no source fallback on a bottle miss) otherwise 404. When a new macOS
# version appears, add its tag here.
EXTRA_LINES=""
for EXTRA_TAG in arm64_sequoia arm64_sonoma; do
  [ "$EXTRA_TAG" = "$TAG" ] && continue
  EXTRA_NAME="$(echo "$PUBLISH_NAME" | sed "s/\.${TAG}\./.${EXTRA_TAG}./")"
  echo "[bottle] uploading $EXTRA_TAG alias $EXTRA_NAME..."
  cp "$BUILT" "$WORK/$EXTRA_NAME"
  upload "$WORK/$EXTRA_NAME"
  EXTRA_LINES="${EXTRA_LINES}    sha256 cellar: :any_skip_relocation, $EXTRA_TAG: \"$SHA256\"\n"
done

echo "[bottle] updating bottle block in $FORMULA..."
REBUILD_LINE=""
[ "$REBUILD" != "0" ] && REBUILD_LINE="    rebuild $REBUILD\n"
BLOCK="  bottle do\n    root_url \"https://github.com/olegshirko/anvil/releases/download/v$VERSION\"\n${REBUILD_LINE}    sha256 cellar: :any_skip_relocation, $TAG: \"$SHA256\"\n${EXTRA_LINES}  end\n"
if grep -q "^  bottle do" "$FORMULA"; then
  BLOCK="$BLOCK" perl -0pi -e 'my $b = $ENV{BLOCK}; $b =~ s/\\n/\n/g; s/  bottle do\n.*?  end\n/$b/s' "$FORMULA"
else
  BLOCK="$BLOCK" perl -0pi -e 'my $b = $ENV{BLOCK}; $b =~ s/\\n/\n/g; s/(  depends_on :macos\n)/$1\n$b/' "$FORMULA"
fi
grep -q "sha256 cellar" "$FORMULA" || { echo "[bottle] error: bottle block not written"; exit 1; }

echo "[bottle] committing and pushing tap..."
cd "$TAP_DIR"
git add anvil.rb
git diff --cached --quiet && { echo "[bottle] formula unchanged, nothing to commit"; exit 0; }
git commit -m "v$VERSION: add $TAG bottle"
git push

echo "[bottle] done. Verify with: brew update && brew reinstall anvil"
