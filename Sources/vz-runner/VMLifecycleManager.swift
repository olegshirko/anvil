import Foundation
import Virtualization

protocol VMLifecycleManagerDelegate: AnyObject {
    func vmLifecycleManagerDidBecomeReady(_ manager: VMLifecycleManager)
    func vmLifecycleManager(_ manager: VMLifecycleManager, didFailWithError error: Error)
    func vmLifecycleManagerDidStop(_ manager: VMLifecycleManager)
}

/// Coarse boot-phase stopwatch: prints `[anvil] phase <name> <ms> (total <ms>)`
/// per mark so daemon.log shows where start/restore time actually goes.
/// Parsed by scripts/time_boot.py.
final class BootPhaseTimer {
    private let lock = NSLock()
    private let start: Date
    private var last: Date

    init() {
        start = Date()
        last = start
    }

    func mark(_ name: String) {
        lock.lock()
        defer { lock.unlock() }
        let now = Date()
        let delta = now.timeIntervalSince(last) * 1000
        let total = now.timeIntervalSince(start) * 1000
        last = now
        print("[anvil] phase \(name.padding(toLength: 14, withPad: " ", startingAt: 0)) \(String(format: "%6.0f", delta)) ms (total \(String(format: "%6.0f", total)) ms)")
    }
}

/// Owns a single VZVirtualMachine and exposes lifecycle operations used by both
/// the one-shot `boot` path and the long-lived `daemon` path.
final class VMLifecycleManager: NSObject {
    let args: BootArgs
    private let snapshot: SnapshotManager
    private var vm: VZVirtualMachine?
    private var coldBootStart: Date?
    /// True when the live VM was booted (not restored) with `args` — only
    /// then may saveSnapshot stamp the config hash. Restored VMs must keep
    /// the hash of the assets they actually booted with; recomputing it
    /// from current files on every save made a rebuilt initramfs look
    /// "matching" and restored a stale guest.
    private var bootedWithCurrentArgs = false

    weak var delegate: VMLifecycleManagerDelegate?

    private let phaseTimer: BootPhaseTimer

    /// Optional host-port availability endpoint; attached to the VM's socket
    /// device before every start/restore. Set by the daemon only.
    var portCheckServer: PortCheckServer?
    /// Outbound connections for the guest through the Mac's network stack.
    let egressServer = EgressServer()
    /// SSH agent forwarding and the Mac's localhost (host.docker.internal).
    let hostServicesServer = HostServicesServer()

    var socketDevice: VZVirtioSocketDevice? {
        vm?.socketDevices.first as? VZVirtioSocketDevice
    }

    /// Install the port-check listener on the live socket device. The device
    /// object exists once the VM is created, before start/restore — which is
    /// the required ordering (the guest may dial port 1027 right after a
    /// snapshot resume, before didBecomeReady fires).
    func attachPortCheckServer() {
        guard let device = socketDevice else { return }
        portCheckServer?.attach(to: device)
        egressServer.attach(to: device)
        hostServicesServer.attach(to: device)
    }

    init(args: BootArgs, phaseTimer: BootPhaseTimer = BootPhaseTimer()) {
        self.args = args
        self.snapshot = SnapshotManager()
        self.phaseTimer = phaseTimer
    }

    // MARK: - Public lifecycle

    /// Set when crash-loop recovery demands a cold boot (the snapshot itself
    /// may be poisoned by a guest that panics right after restore).
    private var forceFreshBoot = false

    /// Start the VM. Tries restore from snapshot first; falls back to cold
    /// boot. Pass `fresh: true` to skip the snapshot entirely (crash-loop
    /// recovery invalidates a possibly poisoned snapshot).
    func start(fresh: Bool = false) {
        forceFreshBoot = fresh
        writeHostTimeFile()
        startHostTimeRefresher()
        configureAndCreateVM { [weak self] result in
            guard let self = self else { return }
            switch result {
            case .failure(let error):
                self.notifyFailure(error)
            case .success(let vm):
                self.vm = vm
                self.phaseTimer.mark("config")
                // The port-check listener must be installed before the VM
                // starts or restores (see attachPortCheckServer); attaching
                // once here also fixes the post-resume race of attaching in
                // didBecomeReady.
                self.attachPortCheckServer()
                self.attemptRestoreOrColdBoot(vm: vm)
            }
        }
    }

    /// How long ensureRunning waits out a transitional VM state (pausing,
    /// saving a snapshot, resuming, restoring) before giving up.
    static let transitionWaitLimit: TimeInterval = 60

    /// Ensure the VM is running: resume it if it is paused, and wait out a
    /// pause or snapshot save that is in progress, then resume. Starting a
    /// stopped VM is not done here: the daemon's crash handler owns restarts
    /// (with backoff), and a second start would open the disk twice.
    func ensureRunning(completion: @escaping (Result<Void, Error>) -> Void) {
        ensureRunning(deadline: Date().addingTimeInterval(Self.transitionWaitLimit), completion: completion)
    }

    private func ensureRunning(deadline: Date, completion: @escaping (Result<Void, Error>) -> Void) {
        DispatchQueue.main.async { [weak self] in
            guard let self = self else { return }
            guard let vm = self.vm else {
                if self.released || self.wakeWaiters != nil {
                    self.wake(completion: completion)
                    return
                }
                completion(.failure(NSError(domain: "anvil", code: 100,
                                            userInfo: [NSLocalizedDescriptionKey: "VM not created yet"])))
                return
            }

            switch vm.state {
            case .running:
                completion(.success(()))
            case .paused:
                self.resume(completion: completion)
            case .stopped, .error:
                completion(.failure(NSError(domain: "anvil", code: 100,
                                            userInfo: [NSLocalizedDescriptionKey: "VM is \(vm.state == .stopped ? "stopped" : "in error state"); restart pending"])))
            default:
                // pausing / saving / resuming / restoring / starting /
                // stopping: an idle pause may have begun just before this
                // client arrived. Poll until the VM settles, then resume —
                // failing here left the client retrying vsock against a
                // paused VM and counted as a crash strike.
                guard Date() < deadline else {
                    completion(.failure(NSError(domain: "anvil", code: 100,
                                                userInfo: [NSLocalizedDescriptionKey: "VM stuck in state \(vm.state.rawValue)"])))
                    return
                }
                DispatchQueue.main.asyncAfter(deadline: .now() + .milliseconds(50)) { [weak self] in
                    self?.ensureRunning(deadline: deadline, completion: completion)
                }
            }
        }
    }

    /// The VM is paused and its committed snapshot already holds exactly
    /// this state (the snapshot is deleted before every resume). Main queue.
    var isPausedWithSavedSnapshot: Bool {
        vm?.state == .paused && snapshot.hasSnapshot
    }

    // MARK: - Memory release

    /// The VM was stopped to give its memory back; the committed snapshot
    /// holds it. Main queue.
    private(set) var released = false
    /// Clients waiting for a released VM to come back (nil: no wake).
    private var wakeWaiters: [(Result<Void, Error>) -> Void]?

    /// Stop an idle-paused VM whose snapshot is saved: a paused VM keeps all
    /// of its memory (2 GiB and more) allocated on the Mac for as long as it
    /// stays paused. The next client restores it from the snapshot.
    func releaseMemory() {
        DispatchQueue.main.async { [weak self] in
            guard let self = self, let vm = self.vm, self.isPausedWithSavedSnapshot, vm.canStop else { return }
            vm.delegate = nil // intentional: not a crash
            vm.stop { [weak self] error in
                DispatchQueue.main.async {
                    guard let self = self else { return }
                    if let error = error {
                        print("[anvil] stopping the idle VM failed: \(error)")
                        return
                    }
                    self.vm = nil
                    self.released = true
                    self.stopHostTimeRefresher()
                    print("[anvil] idle VM stopped to free its memory; the next client restores it")
                }
            }
        }
    }

    /// Bring a released VM back from its snapshot (cold boot if the restore
    /// fails, e.g. while the Mac is locked). The daemon's servers stay as
    /// they are: they reach the new VM through socketDevice.
    private func wake(completion: @escaping (Result<Void, Error>) -> Void) {
        if wakeWaiters != nil {
            wakeWaiters?.append(completion)
            return
        }
        wakeWaiters = [completion]
        print("[anvil] restoring the released VM...")
        writeHostTimeFile()
        startHostTimeRefresher()
        configureAndCreateVM { [weak self] result in
            guard let self = self else { return }
            switch result {
            case .failure(let error):
                self.notifyFailure(error)
            case .success(let vm):
                self.vm = vm
                self.attachPortCheckServer()
                self.attemptRestoreOrColdBoot(vm: vm)
            }
        }
    }

    /// Readiness goes to the clients waiting for a wake, or to the daemon.
    private func notifyReady() {
        if let waiters = wakeWaiters {
            wakeWaiters = nil
            released = false
            waiters.forEach { $0(.success(())) }
            return
        }
        delegate?.vmLifecycleManagerDidBecomeReady(self)
    }

    /// A failed wake fails its clients and is then a crash like any other.
    private func notifyFailure(_ error: Error) {
        if let waiters = wakeWaiters {
            wakeWaiters = nil
            released = false
            waiters.forEach { $0(.failure(error)) }
        }
        delegate?.vmLifecycleManager(self, didFailWithError: error)
    }

    /// Pause the VM.
    func pause(completion: @escaping (Result<Void, Error>) -> Void) {
        DispatchQueue.main.async { [weak self] in
            guard let self = self, let vm = self.vm else {
                completion(.success(()))
                return
            }
            vm.pause { result in
                DispatchQueue.main.async {
                    switch result {
                    case .success:
                        // A paused guest reads nothing: stop rewriting the
                        // time and entropy files every 2 s until resume.
                        self.stopHostTimeRefresher()
                        completion(.success(()))
                    case .failure(let error):
                        completion(.failure(error))
                    }
                }
            }
        }
    }

    /// Resume the VM.
    func resume(completion: @escaping (Result<Void, Error>) -> Void) {
        // Refresh the host time file so the guest re-syncs its clock after
        // resume: while paused the guest clock is frozen, and the file from
        // start() is stale by the whole paused interval.
        writeHostTimeFile()
        startHostTimeRefresher()
        DispatchQueue.main.async { [weak self] in
            guard let self = self, let vm = self.vm else {
                completion(.success(()))
                return
            }
            // The guest writes to /dev/vda as soon as it runs again.
            self.snapshot.invalidateBeforeResume()
            vm.resume { result in
                DispatchQueue.main.async {
                    switch result {
                    case .success:
                        completion(.success(()))
                    case .failure(let error):
                        completion(.failure(error))
                    }
                }
            }
        }
    }

    /// Hard-stop the current VM if it can still run. A restart after a
    /// liveness failure (the guest stopped answering, the VM did not stop)
    /// must never start a second VM on the same containerd disk while the
    /// wedged one keeps writing to it.
    func forceStop(completion: @escaping () -> Void) {
        DispatchQueue.main.async { [weak self] in
            guard let self = self, let vm = self.vm, vm.canStop else {
                completion()
                return
            }
            // The stop is intentional; it must not come back as a crash.
            vm.delegate = nil
            vm.stop { error in
                if let error = error {
                    print("[anvil] force stop failed: \(error)")
                }
                DispatchQueue.main.async { completion() }
            }
        }
    }

    /// Pause, save snapshot, and call completion. Keeps the daemon alive.
    func stopAndSave(completion: @escaping () -> Void) {
        guard vm != nil else {
            completion()
            return
        }
        pause { [weak self] pauseResult in
            guard let self = self else {
                completion()
                return
            }
            switch pauseResult {
            case .failure(let error):
                print("[anvil] pause before stop failed: \(error)")
                completion()
            case .success:
                self.saveSnapshot { _ in
                    completion()
                }
            }
        }
    }

    /// Save the current VM state to snapshot (assumes VM is paused).
    func saveSnapshot(completion: ((Error?) -> Void)? = nil) {
        guard let vm = vm else {
            completion?(nil)
            return
        }
        print("[anvil] saving VM snapshot...")
        snapshot.removeSnapshotStatePreservingSidecars()
        let start = Date()
        vm.saveMachineStateTo(url: snapshot.pendingSnapshotURL) { [weak self] error in
            guard let self = self else {
                completion?(error)
                return
            }
            let duration = Date().timeIntervalSince(start)
            if let error = error {
                print("[anvil] snapshot save failed after \(String(format: "%.3f", duration))s: \(error)")
                self.snapshot.discardPendingSnapshot()
                completion?(error)
                return
            }
            // Stamp the config hash ONLY for a VM booted with these args.
            // A restored VM keeps the hash it booted with — otherwise a
            // rebuilt initramfs looks "matching" and restores a stale guest.
            if self.bootedWithCurrentArgs {
                self.snapshot.writeConfigHash(
                    kernel: self.args.kernelPath,
                    initrd: self.args.initrdPath,
                    cpus: self.args.cpuCount,
                    memory: self.args.memoryGiB,
                    containerdDiskPath: self.args.containerdDiskPath,
                    usersSharePath: hostSharesKey()
                )
            } else {
                print("[anvil] keeping stored config hash (VM was restored, not booted with current assets)")
            }
            // The rename is the commit point: before it there is no snapshot
            // (cold boot), after it a complete one.
            guard self.snapshot.commitPendingSnapshot() else {
                completion?(NSError(domain: "anvil", code: 104,
                                    userInfo: [NSLocalizedDescriptionKey: "snapshot commit failed"]))
                return
            }
            print("[anvil] snapshot saved in \(String(format: "%.3f", duration))s")
            completion?(nil)
        }
    }

    // MARK: - Internals

    /// Write the current host time into the virtiofs share so the guest can
    /// set its clock without NTP: VZ does not guarantee a sane RTC (boots can
    /// start at 1970-01-01), which breaks TLS for registry access. Also write
    /// a random seed for the guest entropy pool: the virt kernel has no
    /// RANDOM_TRUST_CPU and VZ has no virtio-rng device.
    private func writeHostTimeFile() {
        guard let share = args.sharePath, !share.isEmpty else { return }
        let shareURL = URL(fileURLWithPath: share)
        try? "\(Int(Date().timeIntervalSince1970))\n".write(
            to: shareURL.appendingPathComponent(".anvil-host-time"),
            atomically: true, encoding: .utf8)
        var entropy = [UInt8](repeating: 0, count: 64)
        if SecRandomCopyBytes(kSecRandomDefault, entropy.count, &entropy) == errSecSuccess {
            try? Data(entropy).write(to: shareURL.appendingPathComponent(".anvil-host-entropy"), options: .atomic)
        }
    }

    /// Keep refreshing the host time file while the VM is alive: the guest
    /// RTC ticks at an unreliable rate under VZ, and each idle-pause/resume
    /// cycle adds the paused interval to the drift. The guest polls the file
    /// every 5 s and steps its clock forward when it runs more than 1 s
    /// behind; refreshing every 2 s bounds the residual drift to ~2 s.
    private func startHostTimeRefresher() {
        DispatchQueue.main.async { [weak self] in
            guard let self = self, self.hostTimeTimer == nil else { return }
            self.hostTimeTimer = Timer.scheduledTimer(withTimeInterval: 2.0, repeats: true) { [weak self] _ in
                self?.writeHostTimeFile()
            }
        }
    }

    private var hostTimeTimer: Timer?

    private func stopHostTimeRefresher() {
        hostTimeTimer?.invalidate()
        hostTimeTimer = nil
    }

    private func configureAndCreateVM(completion: @escaping (Result<VZVirtualMachine, Error>) -> Void) {
        DispatchQueue.main.async { [weak self] in
            guard let self = self else { return }

            // Drop the previous VM first. A crashed/stopped VM still holds its
            // disk attachments; keeping the reference while creating the next
            // VM leads to multi-minute stalls and "storage device attachment
            // is invalid" on the new instance.
            self.vm = nil

            let hashMatches = self.snapshot.configHashMatches(
                kernel: self.args.kernelPath,
                initrd: self.args.initrdPath,
                cpus: self.args.cpuCount,
                memory: self.args.memoryGiB,
                containerdDiskPath: self.args.containerdDiskPath,
                usersSharePath: hostSharesKey()
            )
            let freshBoot = self.args.fresh || self.forceFreshBoot
            print("[anvil] snapshot exists=\(self.snapshot.hasSnapshot) hashMatches=\(hashMatches) forcedFresh=\(self.forceFreshBoot)")

            var canRestore = self.args.useAgent
                && !freshBoot
                && self.snapshot.hasSnapshot
                && hashMatches

            if freshBoot {
                self.snapshot.removeSnapshot()
            } else if !hashMatches {
                self.snapshot.removeSnapshot()
            }

            let (machineIdentifier, networkConfig) = self.prepareSidecars(
                canRestore: &canRestore
            )

            guard let config = try? makeConfiguration(
                self.args,
                machineIdentifier: machineIdentifier,
                macAddress: networkConfig.macAddresses.first
            ) else {
                completion(.failure(NSError(domain: "anvil", code: 101,
                                            userInfo: [NSLocalizedDescriptionKey: "invalid configuration"])))
                return
            }

            if self.args.useAgent {
                do {
                    try config.validateSaveRestoreSupport()
                    print("[anvil] configuration supports save/restore")
                } catch {
                    print("[anvil] configuration does not support save/restore: \(error)")
                }
            }

            let vm = VZVirtualMachine(configuration: config)
            vm.delegate = self
            completion(.success(vm))
        }
    }

    private func attemptRestoreOrColdBoot(vm: VZVirtualMachine) {
        let hashMatches = snapshot.hasSnapshot && snapshot.configHashMatches(
            kernel: args.kernelPath,
            initrd: args.initrdPath,
            cpus: args.cpuCount,
            memory: args.memoryGiB,
            containerdDiskPath: args.containerdDiskPath,
            usersSharePath: hostSharesKey()
        )
        let canRestore = args.useAgent
            && !args.fresh
            && !forceFreshBoot
            && snapshot.hasSnapshot
            && hashMatches

        if canRestore {
            bootedWithCurrentArgs = false
            print("[anvil] restoring VM from snapshot...")
            let restoreStart = Date()
            vm.restoreMachineStateFrom(url: snapshot.snapshotURL) { [weak self] error in
                guard let self = self else { return }
                let restoreDuration = Date().timeIntervalSince(restoreStart)
                if let error = error {
                    print("[anvil] restore failed after \(String(format: "%.3f", restoreDuration))s: \(error)")
                    if (error as NSError).domain == "VZErrorDomain", (error as NSError).code == 12 {
                        // The saved state is sealed with a Secure Enclave key
                        // that cannot be used while the Mac is locked.
                        print("[anvil] note: a locked Mac cannot decrypt the saved VM state; containers survive the cold boot")
                    }
                    print("[anvil] falling back to cold boot")
                    // Keep the machine identifier and network config: this
                    // VM was configured with them and its next save must stay
                    // restorable. removeSnapshot() deleted them, so the next
                    // start cold-booted again and wiped the containers.
                    self.snapshot.removeSnapshotStatePreservingSidecars()
                    self.coldBoot(vm: vm)
                } else {
                    print("[anvil] VM restored in \(String(format: "%.3f", restoreDuration))s, resuming...")
                    self.phaseTimer.mark("vm_restore")
                    self.snapshot.invalidateBeforeResume()
                    let resumeStart = Date()
                    vm.resume { [weak self] result in
                        DispatchQueue.main.async { [weak self] in
                            guard let self = self else { return }
                            let resumeDuration = Date().timeIntervalSince(resumeStart)
                            switch result {
                            case .success:
                                print("[anvil] VM resumed in \(String(format: "%.3f", resumeDuration))s, streaming console:\n---")
                                self.phaseTimer.mark("vm_resume")
                                self.notifyReady()
                            case .failure(let error):
                                self.notifyFailure(error)
                            }
                        }
                    }
                }
            }
        } else {
            print("[anvil] starting VM (kernel=\(args.kernelPath))...")
            coldBoot(vm: vm)
        }
    }

    private func coldBoot(vm: VZVirtualMachine) {
        coldBootStart = Date()
        bootedWithCurrentArgs = true
        vm.start { [weak self] result in
            DispatchQueue.main.async { [weak self] in
                guard let self = self else { return }
                switch result {
                case .success:
                    if let start = self.coldBootStart {
                        print("[anvil] VM started in \(String(format: "%.3f", Date().timeIntervalSince(start)))s, streaming console:\n---")
                    } else {
                        print("[anvil] VM started, streaming console:\n---")
                    }
                    self.phaseTimer.mark("vm_start")
                    if self.args.useAgent {
                        self.waitForGuestAgent(vm: vm)
                    }
                case .failure(let error):
                    print("[anvil] failed to start: \(error)")
                    self.notifyFailure(error)
                }
            }
        }
    }

    private func waitForGuestAgent(vm: VZVirtualMachine) {
        guard let device = vm.socketDevices.first as? VZVirtioSocketDevice else {
            notifyFailure(NSError(domain: "anvil", code: 102,
                                                                         userInfo: [NSLocalizedDescriptionKey: "virtio socket device not found"]))
            return
        }
        let deadline = Date().addingTimeInterval(120)

        func attempt() {
            guard Date() < deadline else {
                print("[anvil] guest agent did not become ready")
                // Without this the daemon waited forever: no sockets bound,
                // no restart. A guest that panics during boot does not fire
                // VZ's didStop, so this is the only signal.
                self.notifyFailure(NSError(
                    domain: "anvil", code: 103,
                    userInfo: [NSLocalizedDescriptionKey: "guest agent did not become ready within 120 s"]))
                return
            }
            device.connect(toPort: controlPort) { [weak self] result in
                guard let self = self else { return }
                switch result {
                case .success(let conn):
                    conn.close()
                    if let start = self.coldBootStart {
                        print("[anvil] guest agent ready \(String(format: "%.3f", Date().timeIntervalSince(start)))s after VM start")
                    } else {
                        print("[anvil] guest agent ready")
                    }
                    self.phaseTimer.mark("agent")
                    // Declare readiness immediately. Saving the snapshot used
                    // to block here for ~2.4s (pause -> save -> resume); the
                    // daemon already saves on idle timeout and on shutdown,
                    // so an inline save only delayed first use.
                    self.notifyReady()
                case .failure:
                    // Fine-grained: the agent is typically up within a second
                    // of vm.start completing, so a coarse poll interval adds
                    // hundreds of ms of pure slack to cold-boot readiness.
                    DispatchQueue.main.asyncAfter(deadline: .now() + .milliseconds(50)) {
                        attempt()
                    }
                }
            }
        }
        attempt()
    }

    private func prepareSidecars(canRestore: inout Bool) -> (Data, NetworkConfig) {
        let machineIdentifier: Data
        let networkConfig: NetworkConfig

        if canRestore {
            let storedID = snapshot.loadMachineIdentifier()
            let storedNet = snapshot.loadNetworkConfig()

            if storedID == nil {
                print("[anvil] snapshot machine identifier missing, disabling restore")
                canRestore = false
            }
            if storedNet == nil {
                print("[anvil] snapshot network config missing, disabling restore")
                canRestore = false
            }

            if !canRestore {
                snapshot.removeSnapshot()
                machineIdentifier = VZGenericMachineIdentifier().dataRepresentation
                snapshot.saveMachineIdentifier(machineIdentifier)
                networkConfig = NetworkConfig(macAddresses: [VZMACAddress.randomLocallyAdministered().string])
                snapshot.saveNetworkConfig(networkConfig)
                print("[anvil] generated new machine identifier and network config")
            } else {
                machineIdentifier = storedID!
                networkConfig = storedNet!
                print("[anvil] using stored machine identifier for restore")
                print("[anvil] using stored network config for restore")
            }
        } else {
            machineIdentifier = VZGenericMachineIdentifier().dataRepresentation
            snapshot.saveMachineIdentifier(machineIdentifier)
            networkConfig = NetworkConfig(macAddresses: [VZMACAddress.randomLocallyAdministered().string])
            snapshot.saveNetworkConfig(networkConfig)
            print("[anvil] generated new machine identifier and network config")
        }

        return (machineIdentifier, networkConfig)
    }
}

extension VMLifecycleManager: VZVirtualMachineDelegate {
    func guestDidStop(_ virtualMachine: VZVirtualMachine) {
        print("\n[anvil] guest stopped")
        delegate?.vmLifecycleManagerDidStop(self)
    }

    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        print("\n[anvil] VM stopped with error: \(error)")
        delegate?.vmLifecycleManager(self, didFailWithError: error)
    }
}
