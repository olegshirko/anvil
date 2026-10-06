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
    }

    func stop() {
        timer?.invalidate()
        timer = nil
        checkScheduled?.cancel()
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
        guard current != lastInterface else { return }
        print("[nat] Mac primary interface \(lastInterface ?? "none") -> \(current ?? "none")")
        lastInterface = current
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
    private func check(reason: String) {
        guard manager.isVMRunning else {
            pendingCheck = true
            return
        }
        pendingCheck = false
        DispatchQueue.global().async { [weak self] in
            guard let self = self else { return }
            let before = NATRecovery.guestEgress()
            if before == "ok" {
                print("[nat] \(reason): the VM still reaches the internet")
                return
            }
            DispatchQueue.main.async {
                guard self.manager.reattachNAT() else { return }
                print("[nat] \(reason): VM egress \(before); reattached the NAT")
                DispatchQueue.global().asyncAfter(deadline: .now() + self.settleDelay) {
                    let after = NATRecovery.guestEgress()
                    if after == "ok" {
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
