# Security policy

## Reporting a vulnerability

The control (`~/.anvil-vz/control.sock`) and Docker (`~/.anvil-vz/docker.sock`)
sockets are unauthenticated by design: the trust model is the local user
(see the "Current limitations" section of the README). Do not expose them
over the network.

If you found a security-relevant bug, please report it privately:
open a [security advisory](https://github.com/olegshirko/anvil/security/advisories/new).

## Sandbox posture

Unprivileged containers run under Docker's default seccomp profile
(containerd's port of it, capability-aware); `--security-opt
seccomp=unconfined` and `--privileged` lift it, and custom profiles in
Docker's format are honored. The guest has no LSM: `--security-opt
apparmor=…`/`label=…` are rejected at create time.

Isolation between containers and the macOS host rests on the
Virtualization.framework VM boundary (shared memory, virtio devices,
network NAT) plus namespaces, cgroups and seccomp inside the guest. Mind
what crosses that boundary on purpose:

- `/Users` (unless `ANVIL_SHARE_USERS=0`) and the anvil state directory
  are shared into the VM; bind mounts from them are writable by the
  container like any bind mount.
- `host.docker.internal` reaches services on the Mac's loopback, and
  `/run/host-services/ssh-auth.sock` (when mounted) reaches the Mac's
  ssh-agent — as with Docker Desktop.
- `docker cp` and `docker export` work on the container's filesystem from
  a chrooted thread of the guest-agent, so symlinks inside a running
  container cannot redirect them into the VM or its shares.

Treat untrusted images accordingly.
