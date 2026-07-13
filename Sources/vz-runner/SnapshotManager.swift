import Foundation
import CryptoKit

struct SnapshotManager {
    let snapshotURL: URL
    let configHashURL: URL
    let machineIDURL: URL
    let networkConfigURL: URL

    init(name: String = "default") {
        let dir = stateDir.appendingPathComponent("snapshots", isDirectory: true)
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        snapshotURL = dir.appendingPathComponent("\(name).vzstate")
        configHashURL = dir.appendingPathComponent("\(name).config-hash")
        machineIDURL = dir.appendingPathComponent("\(name).machine-id")
        networkConfigURL = dir.appendingPathComponent("\(name).network-config")
    }

    var hasSnapshot: Bool {
        FileManager.default.fileExists(atPath: snapshotURL.path)
            && FileManager.default.fileExists(atPath: configHashURL.path)
    }

    func configHashMatches(kernel: String, initrd: String, cpus: Int, memory: UInt64, containerdDiskPath: String?) -> Bool {
        guard let stored = try? String(contentsOf: configHashURL, encoding: .utf8) else {
            return false
        }
        let components = configHashComponents(kernel: kernel, initrd: initrd, cpus: cpus, memory: memory, containerdDiskPath: containerdDiskPath)
        let current = components.hash
        let storedClean = stored.trimmingCharacters(in: .whitespacesAndNewlines)
        print("[anvil] stored config hash: \(storedClean)")
        print("[anvil] current config hash: \(current)")
        print("[anvil] hash inputs -> kernel:\(components.kernelSHA) initrd:\(components.initrdSHA) cpus:\(components.cpus) memory:\(components.memory) disk:\(components.diskToken)")
        return storedClean == current
    }

    @discardableResult
    func writeConfigHash(kernel: String, initrd: String, cpus: Int, memory: UInt64, containerdDiskPath: String?) -> Bool {
        let components = configHashComponents(kernel: kernel, initrd: initrd, cpus: cpus, memory: memory, containerdDiskPath: containerdDiskPath)
        print("[anvil] writing config hash: \(components.hash)")
        print("[anvil] hash inputs -> kernel:\(components.kernelSHA) initrd:\(components.initrdSHA) cpus:\(components.cpus) memory:\(components.memory) disk:\(components.diskToken)")
        do {
            try components.hash.write(to: configHashURL, atomically: true, encoding: .utf8)
            return true
        } catch {
            print("[anvil] failed to write config hash: \(error)")
            return false
        }
    }

    private struct HashComponents {
        let kernelSHA: String
        let initrdSHA: String
        let cpus: Int
        let memory: UInt64
        let diskToken: String
        let hash: String
    }

    private func configHashComponents(kernel: String, initrd: String, cpus: Int, memory: UInt64, containerdDiskPath: String?) -> HashComponents {
        let kernelSHA = sha256OfFile(kernel) ?? "missing"
        let initrdSHA = sha256OfFile(initrd) ?? "missing"
        let diskToken = diskToken(for: containerdDiskPath)
        let input = "\(kernelSHA):\(initrdSHA):\(cpus):\(memory):\(diskToken)"
        let hash = SHA256.hash(data: Data(input.utf8)).compactMap { String(format: "%02x", $0) }.joined()
        return HashComponents(kernelSHA: kernelSHA, initrdSHA: initrdSHA, cpus: cpus, memory: memory, diskToken: diskToken, hash: hash)
    }

    private func diskToken(for path: String?) -> String {
        guard let path = path, !path.isEmpty else {
            return "nodisk"
        }
        let url = URL(fileURLWithPath: path)
        guard let attrs = try? FileManager.default.attributesOfItem(atPath: url.path),
              let size = attrs[.size] as? NSNumber else {
            return "missing:\(path)"
        }
        return "\(path):\(size)"
    }

    private func sha256OfFile(_ path: String) -> String? {
        guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)) else {
            print("[anvil] failed to read file for hash: \(path)")
            return nil
        }
        return SHA256.hash(data: data).compactMap { String(format: "%02x", $0) }.joined()
    }

    func removeSnapshot() {
        try? FileManager.default.removeItem(at: snapshotURL)
        try? FileManager.default.removeItem(at: configHashURL)
        try? FileManager.default.removeItem(at: machineIDURL)
        try? FileManager.default.removeItem(at: networkConfigURL)
    }

    /// Remove the snapshot and config hash files but keep the sidecars
    /// (machine identifier, network config) so a newly saved snapshot remains
    /// restorable with the same virtual hardware.
    func removeSnapshotStatePreservingSidecars() {
        try? FileManager.default.removeItem(at: snapshotURL)
        try? FileManager.default.removeItem(at: configHashURL)
    }

    func loadMachineIdentifier() -> Data? {
        guard let data = try? Data(contentsOf: machineIDURL) else {
            return nil
        }
        return data
    }

    @discardableResult
    func saveMachineIdentifier(_ data: Data) -> Bool {
        do {
            try data.write(to: machineIDURL, options: .atomic)
            return true
        } catch {
            print("[anvil] failed to write machine identifier: \(error)")
            return false
        }
    }

    func loadNetworkConfig() -> NetworkConfig? {
        guard let data = try? Data(contentsOf: networkConfigURL) else {
            return nil
        }
        return try? JSONDecoder().decode(NetworkConfig.self, from: data)
    }

    @discardableResult
    func saveNetworkConfig(_ config: NetworkConfig) -> Bool {
        do {
            let data = try JSONEncoder().encode(config)
            try data.write(to: networkConfigURL, options: .atomic)
            return true
        } catch {
            print("[anvil] failed to write network config: \(error)")
            return false
        }
    }
}

struct NetworkConfig: Codable {
    let macAddresses: [String]
}
