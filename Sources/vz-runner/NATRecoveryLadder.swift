import Foundation

/// What one NAT-recovery incident does after a confirmed failed egress probe.
enum NATRecoveryStep: Equatable {
    /// Connect the VM's network device to a fresh macOS NAT attachment.
    case reattachNAT
    /// Ask the guest to bring its interface down and up and get a fresh
    /// DHCP lease: a restored virtio-net device can hold stale queue state
    /// that only a driver reset clears, and the reattached NAT may not honour
    /// the old lease.
    case resetGuestNetwork
    /// Nothing left to try in this incident.
    case giveUp
}

/// The bounded ladder for one incident.
///
/// A VM restored from a snapshot can lose its layer-2 path in two places: the
/// host's NAT attachment and the guest's virtio-net device. Each rung is tried
/// once per incident, so a full-tunnel VPN that keeps the path dead does not
/// trigger an endless reattach/reset loop, and a healthy VM never resets its
/// network: the ladder only advances on a confirmed failed probe.
struct NATRecoveryLadder: Equatable {
    private(set) var reattached = false
    private(set) var guestReset = false

    /// No rung is left for this incident.
    var isExhausted: Bool { reattached && guestReset }

    /// The next step after a confirmed failed probe.
    mutating func nextAfterFailure() -> NATRecoveryStep {
        if !reattached {
            reattached = true
            return .reattachNAT
        }
        if !guestReset {
            guestReset = true
            return .resetGuestNetwork
        }
        return .giveUp
    }

    /// The guest reaches the internet again, or the Mac's network changed:
    /// the incident is over and both rungs become available again.
    mutating func recovered() {
        reattached = false
        guestReset = false
    }
}
