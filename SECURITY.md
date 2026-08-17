# Security policy

## Reporting a vulnerability

The control (`~/.anvil-vz/control.sock`) and Docker (`~/.anvil-vz/docker.sock`)
sockets are unauthenticated by design: the trust model is the local user
(see the "Current limitations" section of the README). Do not expose them
over the network.

If you found a security-relevant bug, please report it privately:
open a [security advisory](https://github.com/olegshirko/anvil/security/advisories/new).
