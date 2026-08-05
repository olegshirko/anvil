import Foundation

// `anvil doctor` — diagnose the local installation: hypervisor, signing,
// assets, daemon, docker context and API reachability. Exits non-zero when
// any check fails so it can be used in scripts.
func cmdDoctor() {
    var failures = 0
    func check(_ name: String, _ ok: Bool, _ detail: String = "") {
        print(String(format: "[%@] %@", ok ? "OK" : "FAIL", name) + (detail.isEmpty ? "" : " — \(detail)"))
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

    // Persistent containerd disk + free space on the host volume.
    let diskPath = stateDir.appendingPathComponent("containerd-disk.img").path
    if let attrs = try? FileManager.default.attributesOfItem(atPath: diskPath),
       let size = attrs[.size] as? NSNumber {
        check("containerd disk", true, "\(size.int64Value / (1024*1024*1024)) GiB sparse at \(diskPath)")
    } else {
        check("containerd disk", false, "missing at \(diskPath) (created on first start)")
    }
    if let sysAttrs = try? FileManager.default.attributesOfFileSystem(forPath: NSHomeDirectory()),
       let free = sysAttrs[.systemFreeSize] as? NSNumber {
        let freeGiB = free.int64Value / (1024*1024*1024)
        check("host free space", freeGiB >= 10, "\(freeGiB) GiB free")
    }

    // Snapshot state.
    let snapshotPresent = FileManager.default.fileExists(
        atPath: stateDir.appendingPathComponent("snapshots").path)
    check("snapshot", true, snapshotPresent ? "present (fast resume)" : "absent (next start is a cold boot)")

    // Daemon + control channel.
    check("daemon", isDaemonRunning(), isDaemonRunning() ? "running" : "not running (anvil start)")

    // Docker CLI integration.
    let ctx = shell("docker", "context", "show").trimmingCharacters(in: .whitespacesAndNewlines)
    check("docker context", ctx == "anvil", "current: \(ctx)")
    check("docker.sock", FileManager.default.fileExists(atPath: dockerSocketPath), dockerSocketPath)
    if FileManager.default.fileExists(atPath: dockerSocketPath) {
        let ping = shell("curl", "-sf", "--unix-socket", dockerSocketPath, "http://localhost/_ping")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        check("docker api", ping == "OK", ping.isEmpty ? "no answer on /_ping" : ping)
    }

    // Host /Users share for bind mounts.
    check("/Users share", usersSharePath() != nil,
          usersSharePath() != nil ? "/Users available in the guest" : "disabled (ANVIL_SHARE_USERS=0)")

    if failures > 0 {
        print("\n\(failures) check(s) failed")
        exit(1)
    }
    print("\nall checks passed")
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
