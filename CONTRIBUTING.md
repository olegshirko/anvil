# Contributing

PRs are welcome. Before non-trivial changes, read `ARCHITECTURE.md` — the
project has a few invariants (deterministic container IDs, `/wait` header
timing, the port-publishing architecture) that are easy to break by
accident and all covered by tests.

## Checklist

- `make unit-tests` — Go + Swift unit tests (no VM needed)
- `make dev-rebuild && make dev-integration` — the same against a second
  instance (`ANVIL_INSTANCE=dev`, `~/.anvil-vz-dev`, docker context
  `anvil-dev`), so rebuilds, cold boots and test runs leave the usual
  service and its containers alone; `make dev-validate`, `make dev-stop`.
- `make service-debug-rebuild && make integration` — full integration
  suite against a live VM (pulls alpine/nginx/busybox)
- English only in code comments and commit messages

## The Docker API subset

Anvil implements the slice of the Docker API that `docker` and
`docker compose` actually use. If you're adding an endpoint, check how the
CLI calls it (the CLI often depends on response details that aren't in the
API docs — e.g. `/_ping` headers, `/wait` streaming behavior).

## VM tests in CI

GitHub-hosted macOS runners have no hypervisor, so the VM-booting checks
(`scripts/validate_robustness.py` and `scripts/integration_tests.py`) run in
the `validate` job of `.github/workflows/go.yml` on a self-hosted Apple
Silicon runner with the `anvil-vm` label.

Register a Mac once (`gh auth login` with repository admin rights):

```sh
scripts/setup-ci-runner.sh
```

The script installs the runner service and writes `ANVIL_INSTANCE=ci` into
the runner's environment: every job gets its own `~/.anvil-vz-ci` state,
snapshot, disk and sockets, so the machine's usual anvil service, containers
and docker context are untouched. It deliberately does not change the screen
lock: keep the Mac awake and unlocked while jobs run — Virtualization.framework
cannot restore a snapshot while the screen is locked, and the save/resume
checks would quietly degrade into cold boots. The job sets
`ANVIL_REQUIRE_RESUME=1`, so that case fails with a message instead of
passing.

The job runs:

- on pushes to `main` — robustness checks + the smoke slice of the Docker API
  suite;
- nightly at 03:00 UTC and on `workflow_dispatch` — the full suites;
- on release tags, in parallel with the release (see below).

Pull requests never run on the self-hosted runner: it must not execute
untrusted fork code. To check a branch, dispatch the workflow on it:

```sh
gh workflow run Release --ref <branch>
```

The release job does not wait for `validate` yet, because the runner is a
workstation that can be offline. Add `validate` to the release job's `needs`
once it is a dedicated, always-on Mac.

Locally, `make smoke` runs a few-minute cross-section of the integration
suite (every branch), `make integration` the whole suite, `make validate` the
robustness checks, and `make ci-vm` the same robustness + smoke pair the CI
job runs against the isolated `ci` instance.
