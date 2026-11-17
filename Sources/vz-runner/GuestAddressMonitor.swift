import Foundation

/// Warns when another VM claims this VM's guest address.
///
/// Two anvil instances share one macOS NAT (bridge100). The vmnet DHCP server
/// can hand the same guest address to a second VM; the guests cannot see each
/// other at layer 2 (the NAT isolates them), but the Mac's ARP entry for that
/// address then flaps between the two MACs, and host→guest forwarding breaks
/// intermittently — published ports answer on one instance and time out on the
/// other, `docker pull` hangs — while each guest's own state looks correct.
/// The only place the conflict is visible is here: ask the guest for its
/// leased address, read the Mac's ARP entry for it, compare with this VM's MAC.
///
/// Lease renewal (the initramfs runs udhcpc resident) is the fix that keeps
/// this from happening; this monitor is the warning that says it happened.
final class GuestAddressMonitor {
    /// One `arp` lookup a minute: the conflict persists for minutes, so a
    /// faster poll would only cost processes. The guest is asked on the same
    /// tick, through an internal (non-client) control request, so a paused VM
    /// is not woken.
    private let interval: TimeInterval
    private let snapshot: SnapshotManager
    private let procPath: String
    private var timer: Timer?
    /// The last reported "ip mac" pair: one warning per distinct claim, not
    /// one a minute while the other VM holds the address.
    private var lastReported: String?

    init(interval: TimeInterval = 60,
         snapshot: SnapshotManager = SnapshotManager(),
         procPath: String = "/usr/sbin/arp") {
        self.interval = interval
        self.snapshot = snapshot
        self.procPath = procPath
    }

    func start() {
        guard timer == nil else { return }
        let timer = Timer.scheduledTimer(withTimeInterval: interval, repeats: true) { [weak self] _ in
            self?.check()
        }
        timer.tolerance = interval / 6
        self.timer = timer
    }

    func stop() {
        timer?.invalidate()
        timer = nil
        lastReported = nil
    }

    /// One check. Visible for the daemon's own verification path.
    func check() {
        guard let mac = snapshot.loadNetworkConfig()?.macAddresses.first else { return }
        guard let ip = try? ControlClient.requestInternal("status").guestIP, !ip.isEmpty else {
            return // no lease yet, or the VM is paused
        }
        guard let arpMAC = Self.mac(for: ip, using: procPath) else {
            return // not in the table (never contacted) — nothing to compare
        }
        if Self.macEquals(arpMAC, mac) {
            lastReported = nil
            return
        }
        let claim = "\(ip) \(arpMAC.lowercased())"
        if lastReported != claim {
            lastReported = claim
            let line = "[anvil] WARNING: guest address \(ip) is claimed by \(arpMAC), "
                + "not this VM (\(mac)); another instance was given the same address. "
                + "Published ports and image pulls will fail intermittently until one "
                + "of the VMs is restarted. Check `arp -n \(ip)`."
            FileHandle.standardError.write(Data((line + "\n").utf8))
            if let console = FileHandle(forWritingAtPath: "/dev/console") {
                console.write(Data(("[anvil] " + line + "\n").utf8))
                try? console.close()
            }
        }
    }

    // MARK: - ARP table

    /// The MAC the Mac's ARP table has for `ip`, or nil when the address is
    /// not resolved yet (`(incomplete)`) or the lookup fails.
    static func mac(for ip: String, using procPath: String = "/usr/sbin/arp") -> String? {
        guard let output = run(procPath, ["-n", ip]) else { return nil }
        return parse(output, ip: ip)
    }

    /// Parses `/usr/sbin/arp -n <ip>`:
    /// `? (192.168.64.2) at fe:50:fb:5e:75:0c on bridge100 ifscope [ethernet]`
    /// and the unresolved form `? (192.168.64.2) at (incomplete) on bridge100 ...`.
    /// Pure so it is unit-tested; a different address in the output means the
    /// line belongs to another entry and is ignored.
    static func parse(_ output: String, ip: String) -> String? {
        for line in output.split(separator: "\n") {
            guard line.contains("(\(ip))") else { continue }
            guard let atRange = line.range(of: " at ") else { continue }
            let rest = line[atRange.upperBound...]
            guard let token = rest.split(separator: " ").first else { continue }
            let candidate = String(token)
            guard candidate != "(incomplete)",
                  candidate.filter({ $0 == ":" }).count == 5,
                  candidate.allSatisfy({ $0.isHexDigit || $0 == ":" })
            else { continue }
            return candidate
        }
        return nil
    }

    /// Compares two MACs by value, not by text: macOS prints ARP entries
    /// without leading zeros (`8e:d3:39:2:6a:67`) while the VZ API hands out
    /// `8e:d3:39:02:6a:67`, so a string comparison would warn on every check.
    static func macEquals(_ a: String, _ b: String) -> Bool {
        guard let x = macBytes(a), let y = macBytes(b) else { return false }
        return x == y
    }

    static func macBytes(_ s: String) -> [UInt8]? {
        let parts = s.split(separator: ":")
        guard parts.count == 6 else { return nil }
        var out: [UInt8] = []
        for part in parts {
            guard let byte = UInt8(part, radix: 16) else { return nil }
            out.append(byte)
        }
        return out
    }

    private static func run(_ path: String, _ args: [String]) -> String? {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: path)
        process.arguments = args
        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = FileHandle.nullDevice
        do {
            try process.run()
        } catch {
            return nil
        }
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        guard process.terminationStatus == 0 else { return nil }
        return String(data: data, encoding: .utf8)
    }
}
