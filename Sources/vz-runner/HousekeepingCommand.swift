import Foundation

// `anvil prune` and `anvil disk-compact`: the housekeeping that used to
// exist only as Makefile targets, so Homebrew installs had no way to reclaim
// space.

let containerdDiskURL = stateDir.appendingPathComponent("containerd-disk.img")

/// Run docker against anvil's socket (never the user's current context),
/// streaming its output. Returns the exit status.
@discardableResult
func dockerOnAnvil(_ args: [String]) -> Int32 {
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    proc.arguments = ["docker"] + args
    var env = ProcessInfo.processInfo.environment
    env["DOCKER_HOST"] = "unix://\(dockerSocketPath)"
    env.removeValue(forKey: "DOCKER_CONTEXT")
    proc.environment = env
    do {
        try proc.run()
    } catch {
        print("[anvil] cannot run docker: \(error)")
        return 127
    }
    proc.waitUntilExit()
    return proc.terminationStatus
}

/// `anvil prune [-f]`: remove every container, then unused images, volumes,
/// networks and the build cache inside the VM.
func cmdPrune(args: [String]) {
    guard isDaemonRunning() else {
        print("[anvil] daemon not running; start it with `anvil start`")
        exit(1)
    }
    let force = args.contains("-f") || args.contains("--force")
    if !force {
        print("This removes ALL containers (running ones too), unused images, volumes, networks and the build cache in anvil.")
        print("Continue? [y/N] ", terminator: "")
        guard let answer = readLine(), ["y", "yes"].contains(answer.lowercased()) else {
            print("[anvil] aborted")
            exit(1)
        }
    }
    let ids = shellEnv(["docker", "ps", "-aq"]).split(separator: "\n").map(String.init)
    if !ids.isEmpty {
        dockerOnAnvil(["rm", "-f"] + ids)
    }
    dockerOnAnvil(["volume", "prune", "-af"])
    dockerOnAnvil(["network", "prune", "-f"])
    dockerOnAnvil(["image", "prune", "-af"])
    dockerOnAnvil(["builder", "prune", "-af"])
    print("[anvil] pruned. Run `anvil disk-compact` to give the freed space back to macOS.")
}

/// docker output against anvil's socket, stderr discarded.
private func shellEnv(_ args: [String]) -> String {
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    proc.arguments = args
    var env = ProcessInfo.processInfo.environment
    env["DOCKER_HOST"] = "unix://\(dockerSocketPath)"
    env.removeValue(forKey: "DOCKER_CONTEXT")
    proc.environment = env
    let pipe = Pipe()
    proc.standardOutput = pipe
    proc.standardError = FileHandle.nullDevice
    try? proc.run()
    let data = pipe.fileHandleForReading.readDataToEndOfFile()
    proc.waitUntilExit()
    return String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
}

/// `anvil disk-compact`: rewrite the sparse containerd disk so blocks the
/// guest freed (and trimmed) stop occupying space on the Mac. The logical
/// size and content stay identical, so the VM snapshot stays valid. The
/// daemon is stopped for the copy; it is not restarted here, since whoever
/// started it (`anvil start`, the LaunchAgent, `make service-start`) knows
/// its flags.
func cmdDiskCompact() {
    let disk = containerdDiskURL.path
    guard FileManager.default.fileExists(atPath: disk) else {
        print("[anvil] no containerd disk at \(disk)")
        exit(1)
    }
    let wasRunning = isDaemonRunning()
    if wasRunning {
        cmdStop(args: [])
        guard !isDaemonRunning() else {
            print("[anvil] daemon still running; not touching its disk")
            exit(1)
        }
    }
    let before = allocatedBytes(disk)
    let tmp = disk + ".new"
    print("[anvil] compacting \(disk) (\(formatBytes(before)) allocated)...")
    let dd = Process()
    dd.executableURL = URL(fileURLWithPath: "/bin/dd")
    dd.arguments = ["if=\(disk)", "of=\(tmp)", "conv=sparse", "bs=16m"]
    dd.standardError = FileHandle.nullDevice
    var ok = false
    if (try? dd.run()) != nil {
        dd.waitUntilExit()
        ok = dd.terminationStatus == 0
    }
    if ok, (try? FileManager.default.replaceItemAt(URL(fileURLWithPath: disk), withItemAt: URL(fileURLWithPath: tmp))) != nil {
        chmod(disk, 0o600)
        print("[anvil] compacted: \(formatBytes(allocatedBytes(disk))) allocated")
    } else {
        try? FileManager.default.removeItem(atPath: tmp)
        print("[anvil] compaction failed; disk left untouched")
    }
    if wasRunning {
        print("[anvil] the daemon was stopped for the copy; start it again with `anvil start`")
    }
    if !ok {
        exit(1)
    }
}

private func allocatedBytes(_ path: String) -> Int64 {
    var st = stat()
    guard stat(path, &st) == 0 else { return 0 }
    return Int64(st.st_blocks) * 512
}
