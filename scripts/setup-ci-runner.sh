#!/usr/bin/env bash
# Register this Apple Silicon Mac as the repository's self-hosted VM-test
# runner (the `validate` job in .github/workflows/go.yml).
#
#   gh auth login                      # admin rights on the repository
#   scripts/setup-ci-runner.sh
#
# The runner works in a dedicated anvil instance (~/.anvil-vz-ci): the suites
# kill daemons, rewrite snapshots and pull images, and must not touch the
# machine owner's usual instance. Nothing here changes the docker context.
set -euo pipefail

REPO="${REPO:-olegshirko/anvil}"
RUNNER_DIR="${RUNNER_DIR:-$HOME/actions-runner-anvil}"
RUNNER_NAME="${RUNNER_NAME:-$(scutil --get ComputerName 2>/dev/null || hostname)}-anvil-vm"
RUNNER_LABELS="${RUNNER_LABELS:-anvil-vm}"
INSTANCE="${ANVIL_CI_INSTANCE:-ci}"

fail() { echo "error: $*" >&2; exit 1; }
say() { echo "[ci-runner] $*"; }

[[ "$(uname -s)" == "Darwin" && "$(uname -m)" == "arm64" ]] \
    || fail "Virtualization.framework needs an Apple Silicon Mac"
for tool in gh swift python3 docker codesign tar curl; do
    command -v "$tool" >/dev/null 2>&1 || fail "missing $tool"
done
docker compose version >/dev/null 2>&1 || fail "the docker compose CLI plugin is missing"
docker buildx version >/dev/null 2>&1 || fail "the docker buildx CLI plugin is missing"
gh auth status >/dev/null 2>&1 || fail "run 'gh auth login' first"

version="$(gh api repos/actions/runner/releases/latest --jq .tag_name)"
version="${version#v}"
tarball="actions-runner-osx-arm64-${version}.tar.gz"
sha="$(gh api repos/actions/runner/releases/latest --jq .body \
    | sed -n 's/.*BEGIN SHA osx-arm64 -->\([0-9a-f]\{64\}\)<!--.*/\1/p' | head -1)"
[[ -n "$sha" ]] || fail "cannot read the runner checksum from the release notes"

mkdir -p "$RUNNER_DIR"
if [[ ! -x "$RUNNER_DIR/run.sh" ]]; then
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT
    say "downloading runner $version"
    curl -fL --retry 3 --retry-delay 2 -o "$tmp/$tarball" \
        "https://github.com/actions/runner/releases/download/v${version}/${tarball}"
    actual="$(shasum -a 256 "$tmp/$tarball" | awk '{print $1}')"
    [[ "$actual" == "$sha" ]] || fail "checksum mismatch for $tarball"
    tar xzf "$tmp/$tarball" -C "$RUNNER_DIR"
fi

if [[ -f "$RUNNER_DIR/.runner" ]]; then
    say "runner already configured in $RUNNER_DIR"
else
    token="$(gh api -X POST "repos/$REPO/actions/runners/registration-token" --jq .token)" \
        || fail "cannot create a registration token (repository admin rights needed)"
    say "registering $RUNNER_NAME with labels self-hosted,macOS,ARM64,$RUNNER_LABELS"
    "$RUNNER_DIR/config.sh" --unattended --replace \
        --url "https://github.com/$REPO" \
        --token "$token" \
        --name "$RUNNER_NAME" \
        --labels "$RUNNER_LABELS"
fi

# The suites expect an isolated instance: validate_robustness.py and
# integration_tests.py read ANVIL_INSTANCE themselves, and the daemon and
# docker socket follow it.
printf 'ANVIL_INSTANCE=%s\n' "$INSTANCE" > "$RUNNER_DIR/.env"

# launchd starts the agent with a minimal PATH, while the job needs Homebrew's
# docker CLI and python3. runsvc.sh exports .path before the listener starts.
printf '/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin\n' > "$RUNNER_DIR/.path"

if ls "$HOME/Library/LaunchAgents"/actions.runner.*.plist >/dev/null 2>&1; then
    (cd "$RUNNER_DIR" && ./svc.sh stop) || true
    (cd "$RUNNER_DIR" && ./svc.sh start) || true
else
    (cd "$RUNNER_DIR" && ./svc.sh install)
    (cd "$RUNNER_DIR" && ./svc.sh start)
fi
(cd "$RUNNER_DIR" && ./svc.sh status) || true

# The service reports "started" as soon as launchd spawns it; the runner is
# only usable once it has connected to GitHub.
status=""
for _ in $(seq 1 30); do
    status="$(gh api "repos/$REPO/actions/runners" \
        --jq ".runners[] | select(.name == \"$RUNNER_NAME\") | .status" 2>/dev/null || true)"
    [[ "$status" == "online" ]] && break
    sleep 1
done
if [[ "$status" == "online" ]]; then
    say "runner is online"
else
    say "warning: runner is not online yet (last status: ${status:-not found})"
    say "check $HOME/Library/Logs/actions.runner.*/stdout.log and stderr.log"
fi

cat <<EOF

Runner "$RUNNER_NAME" is registered in $RUNNER_DIR.
Labels: self-hosted, macOS, ARM64, $RUNNER_LABELS.
CI state lives in ~/.anvil-vz-$INSTANCE; the usual ~/.anvil-vz and the
current docker context are not touched.

The Mac must stay awake and unlocked while a job runs: Virtualization.framework
cannot restore a snapshot while the screen is locked, so the suites silently
fall back to cold boots and the save/resume checks get weaker. If the idle
screen lock is shorter than a job, run the agent under caffeinate yourself
(e.g. \`caffeinate -dimsu $RUNNER_DIR/run.sh\`) or disable the screen lock for
the runner user — the setup deliberately does not change that setting.

Remove the runner with:
    $RUNNER_DIR/svc.sh stop && $RUNNER_DIR/svc.sh uninstall
    $RUNNER_DIR/config.sh remove --token <registration-token>
EOF
