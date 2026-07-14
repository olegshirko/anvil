import Foundation

/// Drops the guest VM's page cache before a snapshot is saved. This shrinks
/// the VM state file because the snapshot includes dirty guest memory, and
/// containerd image data cached in RAM becomes unnecessary when the data is
/// already persisted on the containerd block disk.
enum GuestCacheDropper {
    static func dropCaches() {
        guard let executableURL = Bundle.main.executableURL ?? fallbackExecutableURL() else {
            print("[guest-cache-drop] unable to locate anvil executable")
            return
        }
        let process = Process()
        process.executableURL = executableURL
        process.arguments = ["exec", "sh", "-c", "sync; echo 3 > /proc/sys/vm/drop_caches"]
        process.environment = ProcessInfo.processInfo.environment
        process.environment?["ANVIL_EXIT_ON_PARENT_DEATH"] = "1"
        do {
            try process.run()
            process.waitUntilExit()
            if process.terminationStatus == 0 {
                print("[guest-cache-drop] page cache dropped")
            } else {
                print("[guest-cache-drop] failed with status \(process.terminationStatus)")
            }
        } catch {
            print("[guest-cache-drop] failed to run: \(error)")
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
