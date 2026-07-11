import Foundation
import Virtualization

protocol VMLifecycleManagerDelegate: AnyObject {
    func vmLifecycleManagerDidBecomeReady(_ manager: VMLifecycleManager)
    func vmLifecycleManager(_ manager: VMLifecycleManager, didFailWithError error: Error)
    func vmLifecycleManagerDidStop(_ manager: VMLifecycleManager)
}

/// Owns a single VZVirtualMachine and exposes lifecycle operations used by both
/// the one-shot `boot` path and the long-lived `daemon` path.
final class VMLifecycleManager: NSObject {
    let args: BootArgs
    private let snapshot: SnapshotManager
    private var vm: VZVirtualMachine?
    private var coldBootStart: Date?

    weak var delegate: VMLifecycleManagerDelegate?

    var socketDevice: VZVirtioSocketDevice? {
        vm?.socketDevices.first as? VZVirtioSocketDevice
    }

    init(args: BootArgs) {
        self.args = args
        self.snapshot = SnapshotManager()
    }

    // MARK: - Public lifecycle

    /// Start the VM. Tries restore from snapshot first; falls back to cold boot.
    func start() {
        configureAndCreateVM { [weak self] result in
            guard let self = self else { return }
            switch result {
            case .failure(let error):
                self.delegate?.vmLifecycleManager(self, didFailWithError: error)
            case .success(let vm):
                self.vm = vm
                self.attemptRestoreOrColdBoot(vm: vm)
            }
        }
    }

    /// Ensure the VM is running. If it is paused, resume; if stopped, start.
    func ensureRunning(completion: @escaping (Result<Void, Error>) -> Void) {
        DispatchQueue.main.async { [weak self] in
            guard let self = self else { return }
            guard let vm = self.vm else {
                self.start()
                // The delegate will report readiness asynchronously.
                completion(.success(()))
                return
            }

            switch vm.state {
            case .running:
                completion(.success(()))
            case .paused:
                self.resume(completion: completion)
            case .stopped:
                self.start()
                completion(.success(()))
            default:
                let error = NSError(domain: "vz-runner", code: 100,
                                    userInfo: [NSLocalizedDescriptionKey: "unexpected VM state: \(vm.state)"])
                completion(.failure(error))
            }
        }
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
        DispatchQueue.main.async { [weak self] in
            guard let self = self, let vm = self.vm else {
                completion(.success(()))
                return
            }
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
                print("[vz-runner] pause before stop failed: \(error)")
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
        print("[vz-runner] saving VM snapshot...")
        snapshot.removeSnapshotStatePreservingSidecars()
        let start = Date()
        vm.saveMachineStateTo(url: snapshot.snapshotURL) { [weak self] error in
            guard let self = self else {
                completion?(error)
                return
            }
            let duration = Date().timeIntervalSince(start)
            if let error = error {
                print("[vz-runner] snapshot save failed after \(String(format: "%.3f", duration))s: \(error)")
                completion?(error)
                return
            }
            self.snapshot.writeConfigHash(
                kernel: self.args.kernelPath,
                initrd: self.args.initrdPath,
                cpus: self.args.cpuCount,
                memory: self.args.memoryGiB,
                containerdDiskPath: self.args.containerdDiskPath
            )
            print("[vz-runner] snapshot saved in \(String(format: "%.3f", duration))s")
            completion?(nil)
        }
    }

    // MARK: - Internals

    private func configureAndCreateVM(completion: @escaping (Result<VZVirtualMachine, Error>) -> Void) {
        DispatchQueue.main.async { [weak self] in
            guard let self = self else { return }

            let hashMatches = self.snapshot.configHashMatches(
                kernel: self.args.kernelPath,
                initrd: self.args.initrdPath,
                cpus: self.args.cpuCount,
                memory: self.args.memoryGiB,
                containerdDiskPath: self.args.containerdDiskPath
            )
            print("[vz-runner] snapshot exists=\(self.snapshot.hasSnapshot) hashMatches=\(hashMatches)")

            var canRestore = self.args.useAgent
                && !self.args.fresh
                && self.snapshot.hasSnapshot
                && hashMatches

            if self.args.fresh {
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
                completion(.failure(NSError(domain: "vz-runner", code: 101,
                                            userInfo: [NSLocalizedDescriptionKey: "invalid configuration"])))
                return
            }

            if self.args.useAgent {
                do {
                    try config.validateSaveRestoreSupport()
                    print("[vz-runner] configuration supports save/restore")
                } catch {
                    print("[vz-runner] configuration does not support save/restore: \(error)")
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
            containerdDiskPath: args.containerdDiskPath
        )
        let canRestore = args.useAgent
            && !args.fresh
            && snapshot.hasSnapshot
            && hashMatches

        if canRestore {
            print("[vz-runner] restoring VM from snapshot...")
            let restoreStart = Date()
            vm.restoreMachineStateFrom(url: snapshot.snapshotURL) { [weak self] error in
                guard let self = self else { return }
                let restoreDuration = Date().timeIntervalSince(restoreStart)
                if let error = error {
                    print("[vz-runner] restore failed after \(String(format: "%.3f", restoreDuration))s: \(error)")
                    print("[vz-runner] falling back to cold boot")
                    self.snapshot.removeSnapshot()
                    self.coldBoot(vm: vm)
                } else {
                    print("[vz-runner] VM restored in \(String(format: "%.3f", restoreDuration))s, resuming...")
                    let resumeStart = Date()
                    vm.resume { result in
                        DispatchQueue.main.async { [weak self] in
                            guard let self = self else { return }
                            let resumeDuration = Date().timeIntervalSince(resumeStart)
                            switch result {
                            case .success:
                                print("[vz-runner] VM resumed in \(String(format: "%.3f", resumeDuration))s, streaming console:\n---")
                                self.delegate?.vmLifecycleManagerDidBecomeReady(self)
                            case .failure(let error):
                                self.delegate?.vmLifecycleManager(self, didFailWithError: error)
                            }
                        }
                    }
                }
            }
        } else {
            print("[vz-runner] starting VM (kernel=\(args.kernelPath))...")
            coldBoot(vm: vm)
        }
    }

    private func coldBoot(vm: VZVirtualMachine) {
        coldBootStart = Date()
        vm.start { [weak self] result in
            DispatchQueue.main.async { [weak self] in
                guard let self = self else { return }
                switch result {
                case .success:
                    if let start = self.coldBootStart {
                        print("[vz-runner] VM started in \(String(format: "%.3f", Date().timeIntervalSince(start)))s, streaming console:\n---")
                    } else {
                        print("[vz-runner] VM started, streaming console:\n---")
                    }
                    if self.args.useAgent {
                        self.waitForGuestAgent(vm: vm)
                    }
                case .failure(let error):
                    print("[vz-runner] failed to start: \(error)")
                    self.delegate?.vmLifecycleManager(self, didFailWithError: error)
                }
            }
        }
    }

    private func waitForGuestAgent(vm: VZVirtualMachine) {
        guard let device = vm.socketDevices.first as? VZVirtioSocketDevice else {
            delegate?.vmLifecycleManager(self, didFailWithError: NSError(domain: "vz-runner", code: 102,
                                                                         userInfo: [NSLocalizedDescriptionKey: "virtio socket device not found"]))
            return
        }
        let deadline = Date().addingTimeInterval(120)

        func attempt() {
            guard Date() < deadline else {
                print("[vz-runner] guest agent did not become ready")
                return
            }
            device.connect(toPort: controlPort) { [weak self] result in
                guard let self = self else { return }
                switch result {
                case .success(let conn):
                    conn.close()
                    if let start = self.coldBootStart {
                        print("[vz-runner] guest agent ready \(String(format: "%.3f", Date().timeIntervalSince(start)))s after VM start")
                    } else {
                        print("[vz-runner] guest agent ready")
                    }
                    self.pause { pauseResult in
                        switch pauseResult {
                        case .success:
                            self.saveSnapshot { _ in
                                self.resume { _ in
                                    self.delegate?.vmLifecycleManagerDidBecomeReady(self)
                                }
                            }
                        case .failure(let pauseError):
                            print("[vz-runner] pause for snapshot failed: \(pauseError)")
                            self.delegate?.vmLifecycleManagerDidBecomeReady(self)
                        }
                    }
                case .failure:
                    DispatchQueue.main.asyncAfter(deadline: .now() + .milliseconds(500)) {
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
                print("[vz-runner] snapshot machine identifier missing, disabling restore")
                canRestore = false
            }
            if storedNet == nil {
                print("[vz-runner] snapshot network config missing, disabling restore")
                canRestore = false
            }

            if !canRestore {
                snapshot.removeSnapshot()
                machineIdentifier = VZGenericMachineIdentifier().dataRepresentation
                snapshot.saveMachineIdentifier(machineIdentifier)
                networkConfig = NetworkConfig(macAddresses: [VZMACAddress.randomLocallyAdministered().string])
                snapshot.saveNetworkConfig(networkConfig)
                print("[vz-runner] generated new machine identifier and network config")
            } else {
                machineIdentifier = storedID!
                networkConfig = storedNet!
                print("[vz-runner] using stored machine identifier for restore")
                print("[vz-runner] using stored network config for restore")
            }
        } else {
            machineIdentifier = VZGenericMachineIdentifier().dataRepresentation
            snapshot.saveMachineIdentifier(machineIdentifier)
            networkConfig = NetworkConfig(macAddresses: [VZMACAddress.randomLocallyAdministered().string])
            snapshot.saveNetworkConfig(networkConfig)
            print("[vz-runner] generated new machine identifier and network config")
        }

        return (machineIdentifier, networkConfig)
    }
}

extension VMLifecycleManager: VZVirtualMachineDelegate {
    func guestDidStop(_ virtualMachine: VZVirtualMachine) {
        print("\n[vz-runner] guest stopped")
        delegate?.vmLifecycleManagerDidStop(self)
    }

    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        print("\n[vz-runner] VM stopped with error: \(error)")
        delegate?.vmLifecycleManager(self, didFailWithError: error)
    }
}
