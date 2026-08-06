#!/bin/bash
# Build and publish a Homebrew bottle for an already-published GitHub release.
#
# Usage: scripts/make_bottle.sh <version> [tap-dir]
#
# Flow: reinstall the formula from the tap with --build-bottle, run
# `brew bottle`, upload the tarball to the GitHub release (brew fetches
# bottles with a single dash between name and version, while `brew bottle`
# writes a double dash — the asset is renamed), insert/replace the bottle
# block in the formula, commit and push the tap.
set -euo pipefail

VERSION="${1:?usage: make_bottle.sh <version> [tap-dir]}"
TAP_DIR="${2:-$(cd "$(dirname "$0")/../../homebrew-tap" && pwd)}"
FORMULA="$TAP_DIR/anvil.rb"
FORMULA_REF="olegshirko/tap/anvil"
REPO="olegshirko/anvil"

test -f "$FORMULA" || { echo "[bottle] error: $FORMULA not found"; exit 1; }
command -v gh >/dev/null || { echo "[bottle] error: gh CLI required"; exit 1; }

echo "[bottle] reinstalling $FORMULA_REF with --build-bottle..."
brew uninstall "$FORMULA_REF" 2>/dev/null || true
brew install --build-bottle "$FORMULA_REF"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "[bottle] running brew bottle..."
( cd "$WORK" && brew bottle --json "$FORMULA_REF" > bottle-out.txt )

BUILT="$(ls "$WORK"/anvil--*.bottle* | head -1)"
# anvil--1.0.37.arm64_tahoe.bottle.1.tar.gz -> anvil-1.0.37.arm64_tahoe.bottle.1.tar.gz
PUBLISH_NAME="$(basename "$BUILT" | sed 's/^anvil--/anvil-/')"
TAG="$(basename "$BUILT" | sed -E 's/^anvil--[0-9.]+\.([a-z0-9_]+)\.bottle.*$/\1/')"
SHA256="$(shasum -a 256 "$BUILT" | cut -d' ' -f1)"
REBUILD="$(grep -oE '^    rebuild [0-9]+' "$WORK/bottle-out.txt" | awk '{print $2}' || true)"
REBUILD="${REBUILD:-0}"
echo "[bottle] tag=$TAG rebuild=$REBUILD sha256=$SHA256"

echo "[bottle] uploading $PUBLISH_NAME to release v$VERSION..."
cp "$BUILT" "$WORK/$PUBLISH_NAME"
gh release upload "v$VERSION" "$WORK/$PUBLISH_NAME" --repo "$REPO" --clobber
# Drop a stale double-dash asset from a previous manual upload, if any.
gh release delete-asset "v$VERSION" "$(basename "$BUILT")" --repo "$REPO" --yes 2>/dev/null || true

echo "[bottle] updating bottle block in $FORMULA..."
REBUILD_LINE=""
[ "$REBUILD" != "0" ] && REBUILD_LINE="    rebuild $REBUILD\n"
BLOCK="  bottle do\n    root_url \"https://github.com/olegshirko/anvil/releases/download/v$VERSION\"\n${REBUILD_LINE}    sha256 cellar: :any_skip_relocation, $TAG: \"$SHA256\"\n  end\n"
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
