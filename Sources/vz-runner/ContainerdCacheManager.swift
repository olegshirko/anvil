import Foundation

/// Persists `/var/lib/containerd` to the virtiofs share by running a guest-side
/// sync script via `anvil exec`. This keeps pulled images across cold boots
/// while containerd's runtime root stays on tmpfs.
final class ContainerdCacheManager {
    /// Path on the host, used only for logging.
    private let hostArchivePath: String
    /// Path inside the guest; the virtiofs share is always mounted at /mnt/anvil.
    private let guestArchivePath = "/mnt/anvil/containerd-cache.tar.zst"
    private let executableURL: URL

    init?(sharePath: String?) {
        guard let sharePath = sharePath, !sharePath.isEmpty else {
            return nil
        }
        guard let executableURL = Bundle.main.executableURL ?? Self.fallbackExecutableURL() else {
            return nil
        }
        self.executableURL = executableURL
        self.hostArchivePath = (sharePath as NSString).appendingPathComponent("containerd-cache.tar.zst")
    }

    /// Sync the containerd cache to the host share. Blocks until the guest-side
    /// sync script finishes or fails.
    func sync() {
        let process = Process()
        process.executableURL = executableURL
        process.arguments = ["exec", "anvil-sync-containerd", guestArchivePath]
        // Keep the sync child tied to the daemon's lifetime so it does not outlive
        // a shutdown/snapshot cycle.
        process.environment = ProcessInfo.processInfo.environment
        process.environment?["ANVIL_EXIT_ON_PARENT_DEATH"] = "1"

        do {
            try process.run()
            process.waitUntilExit()
            if process.terminationStatus != 0 {
                print("[containerd-cache] sync exited with status \(process.terminationStatus)")
            } else {
                print("[containerd-cache] synced to \(hostArchivePath)")
            }
        } catch {
            print("[containerd-cache] failed to run sync: \(error)")
        }
    }

    private static func fallbackExecutableURL() -> URL? {
        let path = CommandLine.arguments[0]
        guard !path.isEmpty else { return nil }
        if path.hasPrefix("/") {
            return URL(fileURLWithPath: path)
        }
        return URL(fileURLWithFileSystemRepresentation: path, isDirectory: false, relativeTo: URL(fileURLWithPath: FileManager.default.currentDirectoryPath))
    }
}
