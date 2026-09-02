# Security policy

## Reporting a vulnerability

The control (`~/.anvil-vz/control.sock`) and Docker (`~/.anvil-vz/docker.sock`)
sockets are unauthenticated by design: the trust model is the local user
(see the "Current limitations" section of the README). Do not expose them
over the network.

If you found a security-relevant bug, please report it privately:
open a [security advisory](https://github.com/olegshirko/anvil/security/advisories/new).

## Sandbox posture

Containers inside the anvil VM run with **no seccomp filter**: the
guest-agent does not apply Docker's default seccomp profile (or any LSM
profile — the guest has neither AppArmor nor SELinux). `--security-opt
seccomp=unconfined` is the effective default for every container, and
`--security-opt apparmor=…`/`label=…` are rejected at create time.

The practical consequence: the innermost sandbox layer is the container
runtime's namespaces/cgroups only. Isolation between containers and the
macOS host rests on the Virtualization.framework VM boundary (shared
memory, virtio devices, network NAT) rather than syscall filtering.
Adding a default seccomp profile is on the roadmap; until then, treat
untrusted images accordingly.
