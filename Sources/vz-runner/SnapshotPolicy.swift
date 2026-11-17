import Foundation

/// How the VM snapshot is used at start and stop (`ANVIL_SNAPSHOT`).
///
/// - `auto` (default): restore a snapshot only when the last measured
///   restore was not slower than a cold boot, and skip saving one when it
///   would not be restored. The first save/restore cycle always happens so
///   the numbers exist.
/// - `always`: today's unconditional save/restore.
/// - `never`: neither save nor restore.
enum SnapshotMode: String {
    case auto
    case always
    case never

    /// Parses `ANVIL_SNAPSHOT`; an unknown value falls back to `auto`.
    static func configured(_ raw: String? = anvilSetting("ANVIL_SNAPSHOT")) -> SnapshotMode {
        guard let value = raw?.trimmingCharacters(in: .whitespacesAndNewlines).lowercased(),
              !value.isEmpty else {
            return .auto
        }
        return SnapshotMode(rawValue: value) ?? .auto
    }
}

/// Measured costs for one VM memory size. Restore time tracks the configured
/// RAM, so a memory change invalidates both numbers: `matches` reports that.
struct SnapshotTimings: Codable, Equatable {
    /// VM RAM the measurements belong to.
    var memoryGiB: UInt64
    /// Last cold boot until the guest agent answered, in seconds.
    var coldBootSeconds: Double?
    /// Last snapshot restore (state load), in seconds.
    var restoreSeconds: Double?

    func matches(memoryGiB: UInt64) -> Bool {
        self.memoryGiB == memoryGiB
    }
}

/// Persists the last measured boot/restore durations in the instance's state
/// directory (next to run.json). A missing or unreadable file means "unknown",
/// which the policy treats as "use the snapshot".
struct SnapshotTimingsStore {
    let url: URL

    init(url: URL = stateDir.appendingPathComponent("snapshot-timings.json")) {
        self.url = url
    }

    func load() -> SnapshotTimings? {
        guard let data = try? Data(contentsOf: url) else { return nil }
        return try? JSONDecoder().decode(SnapshotTimings.self, from: data)
    }

    /// Replaces the record; a measurement for another memory size starts a
    /// fresh record because the old numbers no longer describe this VM.
    func save(_ timings: SnapshotTimings) {
        try? FileManager.default.createDirectory(
            at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        if let data = try? JSONEncoder().encode(timings) {
            try? data.write(to: url, options: .atomic)
        }
    }

    func recordColdBoot(_ seconds: Double, memoryGiB: UInt64) {
        var timings = record(for: memoryGiB)
        timings.coldBootSeconds = seconds
        save(timings)
    }

    func recordRestore(_ seconds: Double, memoryGiB: UInt64) {
        var timings = record(for: memoryGiB)
        timings.restoreSeconds = seconds
        save(timings)
    }

    private func record(for memoryGiB: UInt64) -> SnapshotTimings {
        if let existing = load(), existing.matches(memoryGiB: memoryGiB) {
            return existing
        }
        return SnapshotTimings(memoryGiB: memoryGiB)
    }
}

/// Pure snapshot save/restore decision. No I/O: unit-tested without a VM.
enum SnapshotPolicy {
    /// A restore must beat a cold boot by this factor to be worth saving:
    /// restoring is only skipped when it is clearly slower, so noise in one
    /// measurement does not flip the policy.
    static let restoreMargin = 1.15

    /// Whether a restore is expected to be at least as fast as a cold boot.
    /// `nil` when either measurement is unknown for this memory size.
    static func restoreIsWorthIt(timings: SnapshotTimings?, memoryGiB: UInt64) -> Bool? {
        guard let timings = timings, timings.matches(memoryGiB: memoryGiB),
              let boot = timings.coldBootSeconds, boot > 0,
              let restore = timings.restoreSeconds, restore > 0 else {
            return nil
        }
        return restore <= boot * restoreMargin
    }

    /// Whether a start should restore the saved snapshot. Unknown numbers
    /// keep the old behaviour (restore), so the first cycle can measure.
    static func useSnapshot(mode: SnapshotMode, timings: SnapshotTimings?, memoryGiB: UInt64) -> Bool {
        switch mode {
        case .never:
            return false
        case .always:
            return true
        case .auto:
            return restoreIsWorthIt(timings: timings, memoryGiB: memoryGiB) ?? true
        }
    }

    /// Whether a stop/idle pause should save the snapshot. The same rule as
    /// `useSnapshot`: there is no point spending seconds on a save that the
    /// next start will discard.
    static func saveSnapshot(mode: SnapshotMode, timings: SnapshotTimings?, memoryGiB: UInt64) -> Bool {
        switch mode {
        case .never:
            return false
        case .always:
            return true
        case .auto:
            return restoreIsWorthIt(timings: timings, memoryGiB: memoryGiB) ?? true
        }
    }

    /// One log line with both numbers, used on the start decision.
    static func describeRestoreSkip(timings: SnapshotTimings, memoryGiB: UInt64) -> String {
        let boot = timings.coldBootSeconds ?? 0
        let restore = timings.restoreSeconds ?? 0
        return String(
            format: "snapshot would take %.3fs to restore vs %.3fs cold boot (%llu GiB), which is not worth it; discarding it and cold booting",
            restore, boot, memoryGiB)
    }

    /// One log line with both numbers, used on the save decision.
    static func describeSaveSkip(timings: SnapshotTimings, memoryGiB: UInt64) -> String {
        let boot = timings.coldBootSeconds ?? 0
        let restore = timings.restoreSeconds ?? 0
        return String(
            format: "snapshot not saved: restoring it would take %.3fs vs %.3fs cold boot (%llu GiB); stopping fast",
            restore, boot, memoryGiB)
    }
}
