# 6a: networking without the CNI plugin processes

Date: 2026-11-16. Rung 6a of `2026-11-16-stage6-single-digit-ms-design.md`.

## Why

Per container start we exec **five plugin binaries** — `loopback`, `bridge`,
`host-local` (IPAM, invoked by bridge), `portmap`, `firewall` — and `portmap`
and `firewall` exec the `iptables` binary in turn. Measured CNI attach: **10 ms
median, 60 ms tail** on the dev guest; kernel has nftables
(`nft_masq`, `nft_reject` loaded). Kiri does the same work in-process with
rtnetlink + nf_tables: 2.2 ms for veth+bridge.

## Scope of the first increment (6a.1)

Only the hot path: **the default bridge network, IPv4**, no static IP, no
IP range, no IPv6, no secondary networks, no `--internal`, no
`--network container:`. Everything else keeps the plugin path.

In-process replacement for that subset:

1. **`lo` up** inside the netns (netlink).
2. **Bridge**: create/ensure the network's bridge, gateway address, `up`
   (netlink; the name and subnet come from the existing conflist, so
   networks created before this change keep working).
3. **Allocator**: read/write the **same on-disk state** the `host-local`
   plugin uses (`/var/lib/cni/networks/<network>/<ip>` holding the container
   id, plus `last_reserved_ip.0`), so existing allocations stay valid and a
   fallback to the plugin path stays consistent.
4. **veth pair** + MAC + address + default route via the gateway; host end
   attached to the bridge (netlink).
5. **NAT** in our own nftables table (`anvil`): per-network masquerade for
   the subnet, installed **once per network**, plus per-container DNAT only
   for published ports (the host-side publishing path stays as it is —
   `PortForwarder` pushes to the container IP; the in-VM DNAT only serves
   containers reaching a published port through the guest IP).
6. **Teardown**: release the IP, drop the container's nft rules; the veth
   dies with the netns (already the case).

Keep writing the conflist exactly as today: it is the network record
(`docker network inspect`, compose labels, subnet assignment, and the fallback
path all read it). Only the per-container plugin **execution** is replaced.

## Switch and parity

`ANVIL_CNI=plugin` forces the old path; `ANVIL_CNI=native` forces the new one
for the supported subset; default is native-with-fallback (any feature the
subset does not cover goes to the plugin path, invisibly).

Parity checks (all must pass with both settings):

- the networking cases of `make dev-integration` (port publishing TCP/UDP,
  port ranges, cross-container DNS mesh, static IP, ip_range, internal,
  IPv6, `--network container:`, compose isolation);
- no plugin processes on the hot path: during `docker run` the guest must show
  no `bridge`/`host-local`/`portmap`/`firewall` processes (check with a
  `--pid host` debug container);
- `docker network inspect` output unchanged; the port scanner's view
  unchanged; `connect`/`disconnect` events unchanged.

## Target

CNI attach 10–60 ms → **≤2 ms**; API flow ~130 → ~110 ms, and the tail (the
60 ms runs) disappears.

## Anti-goals

- No change to DNS-by-name (the agent's hosts/net-alias layer, not CNI).
- No change to the host-side publishing path.
- No new external binaries in the guest (no `nft`, no `ip`).
- A created-but-not-started container still holds no IP.

## Measured result (2026-11-16)

- `cni-attach` in the guest trace: 12.67 ms (plugin path) → **1.26 ms** (native,
  p90 1.49); no plugin processes in the hot path (82 samples during 6 starts:
  none, positive control with `ANVIL_CNI=plugin` shows all four).
- API-shaped flow: 130 → **82.8 ms** median (min 75.5).
- CLI: median ~198 ms, min 192 — the CLI path now carries ~115 ms that the API
  flow does not; that is the next target (see 6d).
- `make dev-integration`: **150 passed / 4 skipped / 0 failed** (verified twice
  by the main session, once on a clean single-instance environment).

### Environment trap worth remembering

Two anvil VMs on the shared VZ NAT can be handed the **same guest IP**
(`192.168.64.2`): the guest runs `udhcpc -q` once at boot and never renews,
while the vmnet DHCP re-leases a live address to a newer VM. The Mac's ARP
entry then flaps between their MACs and *everything* network-related fails
intermittently — published ports, UDP relay, compose, egress, k8s pulls —
while the guest's own state (addresses, routes, nft rules) is correct.
Diagnosis: compare `ip -4 addr show eth0` across instances and the host ARP
entry; fix: stop the duplicate instance, and as a product fix keep the DHCP
lease alive (drop `-q`, add renewal) and warn on a duplicate address.
