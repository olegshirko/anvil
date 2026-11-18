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
    /// A failed probe is repeated this much later before the NAT is
    /// reattached.
    private let confirmDelay: TimeInterval = 5

    private let manager: VMLifecycleManager
    private let store: SCDynamicStore?
    private var timer: Timer?
    private var lastInterface: String?
    private var pendingCheck = false
    private var checkScheduled: DispatchWorkItem?
    /// Periodic guest probe (no change seen on the Mac).
    private let healthInterval: TimeInterval = 60
    private var lastHealthCheck = Date()
    /// What this incident has already tried: one reattach, then one guest
    /// network reset, then quiet until the guest is healthy or the Mac's
    /// network changes. Main queue.
    private var ladder = NATRecoveryLadder()
    /// An action and its follow-up probe are in flight: a periodic check
    /// that lands meanwhile must not judge the path before the step finished.
    /// Main queue.
    private var recovering = false
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
            self?.ladder.recovered()
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

    /// The VM is running again: run a check a route change left pending, or
    /// look right away when the VM was restored from a snapshot. A restored
    /// virtio-net device can come back without a working layer-2 path, and
    /// waiting for the minute tick only delays the recovery.
    func vmBecameReady() {
        // A VM that just started or was restored is a new incident: a ladder
        // exhausted against the previous VM (or a step it died during) must
        // not silence the recovery of this one.
        ladder.recovered()
        recovering = false
        if pendingCheck {
            scheduleCheck(reason: "network changed while the VM was paused")
        } else if manager.restoredFromSnapshot {
            scheduleCheck(reason: "the VM was restored from its snapshot", quietWhenHealthy: true)
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
        ladder.recovered()
        guard current != nil else { return } // offline: nothing to check yet
        scheduleCheck(reason: "primary interface changed")
    }

    private func scheduleCheck(reason: String, quietWhenHealthy: Bool = false) {
        checkScheduled?.cancel()
        let work = DispatchWorkItem { [weak self] in
            self?.check(reason: reason, quietWhenHealthy: quietWhenHealthy)
        }
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
            var before = NATRecovery.guestEgress()
            if before != "ok" {
                // One failed probe is not a dead NAT: under load (a VM at
                // load 100+ with several k3d clusters) the guest's DNS or
                // connect probe timed out once a minute, and every reattach
                // it caused cut the VM's connections — pulls and builds
                // failed with EOFs and expired buildkit leases. Ask again.
                Thread.sleep(forTimeInterval: self.confirmDelay)
                let again = NATRecovery.guestEgress()
                if again == "ok" {
                    print("[nat] \(reason): one VM egress probe failed (\(before)), the next passed; NAT left alone")
                }
                before = again
            }
            if before == "ok" {
                DispatchQueue.main.async {
                    // A step in flight confirms health itself when its probe
                    // lands; resetting the ladder underneath it would only
                    // let a new incident start mid-step.
                    guard !self.recovering else { return }
                    self.ladder.recovered()
                    if !quietWhenHealthy {
                        print("[nat] \(reason): the VM still reaches the internet")
                    }
                }
                return
            }
            DispatchQueue.main.async {
                // Everything this incident can try was already tried: a
                // full-tunnel VPN keeps the path dead, and reattaching or
                // resetting every minute would only cut connections.
                guard !self.ladder.isExhausted, !self.recovering else { return }
                self.recover(reason: reason, failure: before)
            }
        }
    }

    /// Advances one rung of the incident ladder. Main queue.
    private func recover(reason: String, failure: String) {
        guard manager.isVMRunning, !recovering else { return }
        switch ladder.nextAfterFailure() {
        case .reattachNAT:
            guard manager.reattachNAT() else { return }
            recovering = true
            print("[nat] \(reason): VM egress \(failure); reattached the NAT")
            probeAfterAction(reason: reason)
        case .resetGuestNetwork:
            // A reattached NAT hands the guest a fresh vmnet attachment, but
            // a restored virtio-net device keeps stale queue state and the
            // guest can hold a lease the attachment no longer honours; only a
            // guest-side driver reset and a fresh DHCP lease clear both. The
            // old code stopped here and a restart was the only way out.
            recovering = true
            print("[nat] VM egress still \(failure) after reattaching the NAT; reinitializing the guest network")
            DispatchQueue.global().async { [weak self] in
                guard let self = self else { return }
                let reset = NATRecovery.guestNetworkReset()
                DispatchQueue.main.async { [weak self] in
                    guard let self = self else { return }
                    if let reset = reset {
                        print("[nat] guest network reset failed: \(reset)")
                    } else {
                        print("[nat] guest network reinitialized")
                    }
                    guard self.manager.isVMRunning else {
                        self.recovering = false
                        return
                    }
                    self.probeAfterAction(reason: reason)
                }
            }
        case .giveUp:
            print("[nat] VM egress still \(failure) after reattaching the NAT and reinitializing the guest network\(vpnHint())")
        }
    }

    /// Probes once more after a rung had time to take effect; a still-dead
    /// path moves on to the next rung. Main queue, probing off it.
    private func probeAfterAction(reason: String) {
        DispatchQueue.global().asyncAfter(deadline: .now() + settleDelay) { [weak self] in
            guard let self = self else { return }
            let after = NATRecovery.guestEgress()
            DispatchQueue.main.async { [weak self] in
                guard let self = self else { return }
                self.recovering = false
                guard self.manager.isVMRunning else { return }
                if after == "ok" {
                    self.ladder.recovered()
                    print("[nat] VM internet restored")
                    return
                }
                self.recover(reason: reason, failure: after)
            }
        }
    }

    /// Says why a full-tunnel VPN can keep the VM offline even after both
    /// rungs (macOS NAT traffic does not pass the Mac's default route).
    private func vpnHint() -> String {
        guard let iface = lastInterface, iface.hasPrefix("utun") else { return "" }
        return " (the Mac's default route is the VPN tunnel \(iface); macOS NAT traffic does not pass it — registry traffic still goes through the Mac)"
    }

    /// The guest's direct-egress state: "ok", "via-host", or an error.
    private static func guestEgress() -> String {
        guard let resp = try? ControlClient.requestInternal("egress") else { return "unknown (no answer)" }
        if resp.status == "ok" { return "ok" }
        if resp.status == "via-host" {
            return resp.error.map { "via-host (\($0))" } ?? "via-host"
        }
        return resp.error ?? resp.status ?? "unknown"
    }

    /// Asks the guest to reinitialize eth0 and take a fresh DHCP lease.
    /// Returns nil when the guest reports success, otherwise a one-line
    /// explanation.
    private static func guestNetworkReset() -> String? {
        guard let resp = try? ControlClient.resetGuestNetwork() else { return "no answer" }
        if resp.status == "ok" { return nil }
        return resp.error ?? resp.status ?? "unknown"
    }
}
