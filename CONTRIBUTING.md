# Contributing

PRs are welcome. Before non-trivial changes, read `ARCHITECTURE.md` — the
project has a few invariants (deterministic container IDs, `/wait` header
timing, the port-publishing architecture) that are easy to break by
accident and all covered by tests.

## Checklist

- `make unit-tests` — Go + Swift unit tests (no VM needed)
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
Silicon runner only, when the workflow is started by hand.

Register a runner once, on a Mac with Xcode, Go, Docker CLI and compose:

1. Repository → Settings → Actions → Runners → New self-hosted runner,
   macOS / ARM64. Follow the download and `./config.sh` steps, and keep the
   default labels (`self-hosted`, `macOS`, `ARM64`).
2. `./svc.sh install && ./svc.sh start` to keep it running as a launch agent.
   It must run as a logged-in user: Virtualization.framework does not work
   from a system daemon.
3. Before a release: `gh workflow run Release --ref main` and wait for the
   `validate` job (about 25 minutes).

Locally, `make smoke` runs a few-minute cross-section of the integration
suite (every branch), `make integration` the whole suite and `make validate`
the robustness checks (both before a release).
