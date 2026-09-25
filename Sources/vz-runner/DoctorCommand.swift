import Foundation

// `anvil doctor [--json]` — diagnose the local installation: hypervisor,
// signing, assets, daemon, docker context and API reachability. Exits
// non-zero when any check fails so it can be used in scripts. `--json`
// emits one object with a `checks` array (name/ok/detail) instead of the
// human-readable lines, plus a top-level `ok` for a single glance gate.
func cmdDoctor(args: [String]) {
    let asJSON = args.contains("--json")
    var results: [(name: String, ok: Bool, detail: String)] = []
    var failures = 0
    func check(_ name: String, _ ok: Bool, _ detail: String = "") {
        if !asJSON {
            print(String(format: "[%@] %@", ok ? "OK" : "FAIL", name) + (detail.isEmpty ? "" : " — \(detail)"))
        }
        results.append((name, ok, detail))
        if !ok { failures += 1 }
    }

    // Hypervisor support (Apple Silicon with virtualization available).
    let hv = shell("sysctl", "-n", "kern.hv_support").trimmingCharacters(in: .whitespacesAndNewlines)
    check("hypervisor", hv == "1", "kern.hv_support=\(hv)")

    // The binary must be signed with the virtualization entitlement.
    let ent = shell("codesign", "-d", "--entitlements", ":-", currentExecutablePath())
    check("entitlement", ent.contains("com.apple.security.virtualization"), "com.apple.security.virtualization")

    // Boot assets: kernel + initrd in the state dir, brew assets or the project.
    let kernelCandidates: [String] = [
        Optional(stateDir.appendingPathComponent("vmlinuz-raw").path),
        Optional(stateDir.appendingPathComponent("vmlinuz-raw.gz").path),
        brewAssetsDir().map { "\($0)/vmlinuz-raw" },
        brewAssetsDir().map { "\($0)/vmlinuz-raw.gz" },
        findProjectRoot().map { "\($0)/.download/alpine/vmlinuz-raw" },
        findProjectRoot().map { "\($0)/.download/alpine/vmlinuz-raw.gz" },
    ].compactMap { $0 }
    let initrdCandidates: [String] = [
        Optional(stateDir.appendingPathComponent("initramfs-containerd").path),
        brewAssetsDir().map { "\($0)/initramfs-containerd" },
        findProjectRoot().map { "\($0)/.download/ubuntu/initramfs-containerd" },
    ].compactMap { $0 }
    let kernel = kernelCandidates.first { FileManager.default.fileExists(atPath: $0) }
    let initrd = initrdCandidates.first { FileManager.default.fileExists(atPath: $0) }
    check("kernel", kernel != nil, kernel ?? "not found in ~/.anvil-vz, brew assets or .download")
    check("initramfs", initrd != nil, initrd ?? "not found")

    let daemonRunning = isDaemonRunning()

    // Persistent containerd disk + free space on the host volume. A fresh
    // install has none until the first start creates it: only a running
    // daemon without a disk is a failure.
    let diskPath = stateDir.appendingPathComponent("containerd-disk.img").path
    if let attrs = try? FileManager.default.attributesOfItem(atPath: diskPath),
       let size = attrs[.size] as? NSNumber {
        check("containerd disk", true, "\(size.int64Value / (1024*1024*1024)) GiB sparse at \(diskPath)")
    } else if daemonRunning {
        check("containerd disk", false, "missing at \(diskPath) while the daemon runs")
    } else {
        check("containerd disk", true, "not created yet (created on first start)")
    }
    if let sysAttrs = try? FileManager.default.attributesOfFileSystem(forPath: NSHomeDirectory()),
       let free = sysAttrs[.systemFreeSize] as? NSNumber {
        let freeGiB = free.int64Value / (1024*1024*1024)
        check("host free space", freeGiB >= 10, "\(freeGiB) GiB free")
    }

    // Snapshot state. The snapshots directory always exists; the state file
    // is what a resume needs, and it only exists while the VM is stopped or
    // paused (it is dropped as soon as the VM runs again).
    let snapshotPresent = FileManager.default.fileExists(
        atPath: SnapshotManager().snapshotURL.path)
    let snapshotDetail: String
    if snapshotPresent {
        snapshotDetail = "present (next start resumes)"
    } else if daemonRunning {
        snapshotDetail = "none while the VM runs (saved on stop and idle pause)"
    } else {
        snapshotDetail = "absent (next start is a cold boot)"
    }
    check("snapshot", true, snapshotDetail)

    // Daemon + control channel.
    check("daemon", daemonRunning, daemonRunning ? "running" : "not running (anvil start)")

    // Docker CLI integration: the anvil context, or DOCKER_HOST pointing at
    // the anvil socket, both reach anvil.
    let ctx = shell("docker", "context", "show").trimmingCharacters(in: .whitespacesAndNewlines)
    let dockerHost = ProcessInfo.processInfo.environment["DOCKER_HOST"] ?? ""
    if dockerHost == "unix://\(dockerSocketPath)" {
        check("docker context", true, "DOCKER_HOST=\(dockerHost)")
    } else {
        check("docker context", ctx == "anvil", "current: \(ctx)" + (dockerHost.isEmpty ? "" : ", DOCKER_HOST=\(dockerHost)"))
    }
    check("docker.sock", FileManager.default.fileExists(atPath: dockerSocketPath), dockerSocketPath)
    var ping = ""
    if FileManager.default.fileExists(atPath: dockerSocketPath) {
        ping = shell("curl", "-sf", "--unix-socket", dockerSocketPath, "http://localhost/_ping")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        check("docker api", ping == "OK", ping.isEmpty ? "no answer on /_ping" : ping)
    }

    // Internet from inside the VM. The guest leaves through the macOS NAT; a
    // full-tunnel VPN can swallow that traffic while the Mac stays online, and
    // every pull then fails with a bare i/o timeout.
    if daemonRunning {
        let vpn = defaultRouteInterface(fromRouteOutput: shell("route", "-n", "get", "default"))
            .flatMap { $0.hasPrefix("utun") ? $0 : nil }
        let vpnHint = vpn.map {
            " — the Mac's default route goes through \($0), a full-tunnel VPN (e.g. a Tailscale exit node); " +
            "VM traffic behind the macOS NAT does not pass through it. Turn the exit node off or split-tunnel."
        } ?? ""
        do {
            let resp = try ControlClient.request("egress")
            if resp.status == "ok" {
                check("vm internet", true, "the VM reaches registry-1.docker.io:443")
            } else if let err = resp.error, err.hasPrefix("unknown command") {
                check("vm internet", true, "skipped (guest-agent predates this check)")
            } else {
                check("vm internet", false, (resp.error ?? "no answer") + vpnHint)
            }
        } catch {
            check("vm internet", false, "control socket: \(error)")
        }
    }

    // Host /Users share for bind mounts. Turning it off is a choice, not a
    // failure.
    if usersSharePath() != nil {
        check("/Users share", true, "/Users available in the guest")
    } else if ProcessInfo.processInfo.environment["ANVIL_SHARE_USERS"] == "0" {
        check("/Users share", true, "disabled by ANVIL_SHARE_USERS=0 (bind mounts from /Users will not work)")
    } else {
        check("/Users share", false, "unavailable")
    }

    // Inside-container sanity: a fresh container must have its loopback UP
    // (the CNI loopback plugin's job). Skipped when no local image exists —
    // doctor must stay usable offline. --pull=never keeps the check fast.
    if ping == "OK" {
        // -H: test anvil itself, not whatever engine the current context is.
        let lo = shell("docker", "-H", "unix://\(dockerSocketPath)", "run", "--rm", "--pull=never", "alpine",
                       "ip", "link", "show", "lo")
        let loUp = lo.contains("<LOOPBACK,UP,LOWER_UP>")
        if lo.isEmpty {
            check("container lo", true, "skipped (no local alpine image)")
        } else {
            // Loopback devices report "state UNKNOWN"; UP lives in the
            // interface flags: <LOOPBACK,UP,LOWER_UP>.
            check("container lo", loUp,
                  loUp ? "127.0.0.1 answers in containers" : "lo is DOWN inside containers")
        }
    }

    if asJSON {
        let checks = results.map { r in
            // Strings are plain data (paths, versions); JSON-escaping via
            // JSONSerialization keeps quotes/newlines correct.
            func esc(_ s: String) -> String {
                let d = try! JSONSerialization.data(withJSONObject: [s])
                return String(data: d.dropFirst().dropLast(), encoding: .utf8)!
            }
            return "{\"name\":\(esc(r.name)),\"ok\":\(r.ok),\"detail\":\(esc(r.detail))}"
        }
        print("{\"ok\":\(failures == 0),\"failed\":\(failures),\"checks\":[")
        print(checks.joined(separator: ",\n"))
        print("]}")
    }
    if failures > 0 {
        if !asJSON { print("\n\(failures) check(s) failed") }
        exit(1)
    }
    if !asJSON { print("\nall checks passed") }
}

// `anvil logs [daemon|console|guest]` — tail the relevant log files.
func cmdLogs(args: [String]) {
    let logs: [(String, String)] = [
        ("daemon", stateDir.appendingPathComponent("daemon.log").path),
        ("console", stateDir.appendingPathComponent("console.log").path),
        // guest-agent.log is written to the virtiofs share (debug mode only).
        ("guest", findProjectRoot().map { "\($0)/guest-agent.log" }
            ?? stateDir.appendingPathComponent("guest-agent.log").path),
    ]
    let which = args.first
    for (name, path) in logs where which == nil || which == name {
        print("==> \(name): \(path)")
        guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)),
              let text = String(data: data, encoding: .utf8) else {
            print("(missing)\n")
            continue
        }
        let lines = text.split(separator: "\n", omittingEmptySubsequences: false)
        for line in lines.suffix(30) {
            print(line)
        }
        print("")
    }
}

/// The interface of the default route from `route -n get default` output.
func defaultRouteInterface(fromRouteOutput output: String) -> String? {
    for line in output.split(separator: "\n") {
        let parts = line.split(separator: ":", maxSplits: 1)
        if parts.count == 2, parts[0].trimmingCharacters(in: .whitespaces) == "interface" {
            let iface = parts[1].trimmingCharacters(in: .whitespaces)
            return iface.isEmpty ? nil : iface
        }
    }
    return nil
}
