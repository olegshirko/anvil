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
