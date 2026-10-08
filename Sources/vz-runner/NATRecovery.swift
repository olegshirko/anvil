import AppKit
import Foundation
import SystemConfiguration

/// Brings the VM's internet back after the Mac's network changes under it.
///
/// The VM leaves through a macOS NAT attachment that keeps the routing it
/// was created with: turning a full-tunnel VPN (a Tailscale exit node) on
/// and off left containers without internet until the VM was cold-booted,
/// although the Mac itself was back online. The monitor watches the Mac's
/// primary interface; a few seconds after it changes it asks the guest
/// whether its direct path works ("egress"), and when it does not, connects
/// the network device to a fresh NAT attachment — no restart. A change seen
/// while the VM is paused is checked when the VM is running again.
///
/// The interface does not always change: after a sleep with a VPN on, the
/// Mac lost its 192.168.64.0/24 route to the NAT bridge, and NAT replies to
/// the VM left through Wi-Fi. So the guest is also asked after a wake and
/// once a minute; a dead path gets one reattach per incident (not one a
/// minute while a full-tunnel VPN keeps it dead).
final class NATRecovery {
    /// How often the primary interface is read (a dictionary lookup in
    /// configd, no process spawned).
    private let pollInterval: TimeInterval = 5
    /// Lets the new route and DNS settle before the guest is asked.
    private let settleDelay: TimeInterval = 3

    private let manager: VMLifecycleManager
    private let store: SCDynamicStore?
    private var timer: Timer?
    private var lastInterface: String?
    private var pendingCheck = false
    private var checkScheduled: DispatchWorkItem?
    /// Periodic guest probe (no change seen on the Mac).
    private let healthInterval: TimeInterval = 60
    private var lastHealthCheck = Date()
    /// One reattach per incident: set when the NAT was reattached, cleared
    /// when the guest reaches the internet again or the network changes.
    private var reattachedSinceHealthy = false
    private var wakeObserver: NSObjectProtocol?

    init(manager: VMLifecycleManager) {
        self.manager = manager
        self.store = SCDynamicStoreCreate(nil, "anvil-nat-recovery" as CFString, nil, nil)
    }

    /// Start watching (main queue).
    func start() {
        lastInterface = primaryInterface()
        timer = Timer.scheduledTimer(withTimeInterval: pollInterval, repeats: true) { [weak self] _ in
            self?.poll()
        }
        wakeObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didWakeNotification, object: nil, queue: .main
        ) { [weak self] _ in
            self?.reattachedSinceHealthy = false
            self?.scheduleCheck(reason: "the Mac woke up")
        }
    }

    func stop() {
        timer?.invalidate()
        timer = nil
        checkScheduled?.cancel()
        if let o = wakeObserver {
            NSWorkspace.shared.notificationCenter.removeObserver(o)
            wakeObserver = nil
        }
    }

    /// The VM is running again: run a check a route change left pending.
    func vmBecameReady() {
        if pendingCheck {
            scheduleCheck(reason: "network changed while the VM was paused")
        }
    }

    /// The interface the Mac's IPv4 default route uses (en0, utun5, ...).
    func primaryInterface() -> String? {
        guard let store = store,
              let value = SCDynamicStoreCopyValue(store, "State:/Network/Global/IPv4" as CFString) as? [String: Any]
        else { return nil }
        return value["PrimaryInterface"] as? String
    }

    private func poll() {
        let current = primaryInterface()
        guard current != lastInterface else {
            if current != nil, Date().timeIntervalSince(lastHealthCheck) >= healthInterval {
                lastHealthCheck = Date()
                check(reason: "periodic check", quietWhenHealthy: true)
            }
            return
        }
        print("[nat] Mac primary interface \(lastInterface ?? "none") -> \(current ?? "none")")
        lastInterface = current
        reattachedSinceHealthy = false
        guard current != nil else { return } // offline: nothing to check yet
        scheduleCheck(reason: "primary interface changed")
    }

    private func scheduleCheck(reason: String) {
        checkScheduled?.cancel()
        let work = DispatchWorkItem { [weak self] in self?.check(reason: reason) }
        checkScheduled = work
        DispatchQueue.main.asyncAfter(deadline: .now() + settleDelay, execute: work)
    }

    /// Main queue: asks the guest off the main queue, acts back on it.
    private func check(reason: String, quietWhenHealthy: Bool = false) {
        guard manager.isVMRunning else {
            if !quietWhenHealthy { pendingCheck = true }
            return
        }
        pendingCheck = false
        DispatchQueue.global().async { [weak self] in
            guard let self = self else { return }
            let before = NATRecovery.guestEgress()
            if before == "ok" {
                DispatchQueue.main.async { self.reattachedSinceHealthy = false }
                if !quietWhenHealthy {
                    print("[nat] \(reason): the VM still reaches the internet")
                }
                return
            }
            DispatchQueue.main.async {
                // Once per incident: a full-tunnel VPN keeps the path dead,
                // and reattaching every minute would only cut connections.
                guard !self.reattachedSinceHealthy, self.manager.isVMRunning else { return }
                guard self.manager.reattachNAT() else { return }
                self.reattachedSinceHealthy = true
                print("[nat] \(reason): VM egress \(before); reattached the NAT")
                DispatchQueue.global().asyncAfter(deadline: .now() + self.settleDelay) {
                    let after = NATRecovery.guestEgress()
                    if after == "ok" {
                        DispatchQueue.main.async { self.reattachedSinceHealthy = false }
                        print("[nat] VM internet restored")
                    } else {
                        let iface = DispatchQueue.main.sync { self.lastInterface ?? "" }
                        let hint = iface.hasPrefix("utun")
                            ? " (the Mac's default route is the VPN tunnel \(iface); macOS NAT traffic does not pass it — registry traffic still goes through the Mac)"
                            : ""
                        print("[nat] VM egress still \(after) after reattaching the NAT\(hint)")
                    }
                }
            }
        }
    }

    /// The guest's direct-egress state: "ok", "via-host", or an error.
    private static func guestEgress() -> String {
        guard let resp = try? ControlClient.request("egress") else { return "unknown (no answer)" }
        if resp.status == "ok" { return "ok" }
        return resp.status == "via-host" ? "via-host" : (resp.error ?? resp.status ?? "unknown")
    }
}
