import Foundation
import Virtualization

fileprivate var globalDaemon: DaemonCommand.Daemon?

enum DaemonCommand {
    static func run(args: [String]) {
        setbuf(stdout, nil)
        let cliArgs = parseDaemonArgs(args)

        // Measures the whole daemon spawn -> control-socket-bound window;
        // phase marks land in daemon.log for boot profiling.
        let phaseTimer = BootPhaseTimer()

        guard acquireDaemonLock() else {
            print("[anvil] daemon already running")
            exit(1)
        }
        phaseTimer.mark("lock")

        let manager = VMLifecycleManager(args: cliArgs.bootArgs, phaseTimer: phaseTimer)
        let daemon = Daemon(manager: manager, idleSeconds: cliArgs.idleSeconds, phaseTimer: phaseTimer)
        globalDaemon = daemon

        signal(SIGINT) { _ in
            DispatchQueue.main.async {
                print("\n[anvil] daemon received SIGINT, shutting down...")
                globalDaemon?.shutdown()
            }
        }

        signal(SIGTERM) { _ in
            DispatchQueue.main.async {
                print("\n[anvil] daemon received SIGTERM, shutting down...")
                globalDaemon?.shutdown()
            }
        }

        daemon.start()

        // Keep the main run loop alive even when no timers/clients are active.
        Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { _ in }

        RunLoop.main.run()
    }

    // MARK: - Private

    private static func acquireDaemonLock() -> Bool {
        try? FileManager.default.createDirectory(at: stateDir, withIntermediateDirectories: true)

        let pidFile = daemonPIDFile
        let ownPid = getpid()
        if let data = try? Data(contentsOf: pidFile),
           let s = String(data: data, encoding: .utf8),
           let pid = Int32(s.trimmingCharacters(in: .whitespacesAndNewlines)),
           pid != ownPid,
           kill(pid, 0) == 0 {
            return false
        }

        if let data = String(ownPid).data(using: .utf8) {
            try? data.write(to: pidFile, options: .atomic)
        }
        return true
    }

    private static func releaseDaemonLock() {
        try? FileManager.default.removeItem(at: daemonPIDFile)
        try? FileManager.default.removeItem(atPath: controlSocketPath)
    }

    private struct DaemonCLIArgs {
        var bootArgs: BootArgs
        var idleSeconds: TimeInterval
        var debug: Bool
    }

    private static func parseDaemonArgs(_ raw: [String]) -> DaemonCLIArgs {
        var kernel: String?
        var initrd: String?
        var sharePath: String?
        var mountTag: String = "anvil"
        var cpus: Int = 2
        var memory: UInt64 = 2
        var idleSeconds: TimeInterval = 60
        var containerdDiskPath: String?
        var debug = false

        var it = raw.makeIterator()
        while let a = it.next() {
            switch a {
            case "--kernel":
                kernel = it.next()
            case "--initrd":
                initrd = it.next()
            case "--share":
                sharePath = it.next()
            case "--mount-tag":
                if let v = it.next() { mountTag = v }
            case "--cpus":
                if let v = it.next() { cpus = Int(v) ?? cpus }
            case "--memory":
                if let v = it.next() { memory = UInt64(v) ?? memory }
            case "--idle":
                if let v = it.next(), let n = TimeInterval(v) {
                    idleSeconds = n
                }
            case "--containerd-disk":
                containerdDiskPath = it.next()
            case "--debug":
                debug = true
            default:
                break
            }
        }

        // For the daemon we always use the agent path; fresh boot is handled via --fresh on demand.
        return DaemonCLIArgs(
            bootArgs: BootArgs(
                // The Alpine linux-virt kernel is the pairing the packed
                // modules match; the Ubuntu generic kernel under
                // .download/ubuntu cannot load them (vsock/virtio fail and
                // the guest panics).
                kernelPath: kernel ?? ".download/alpine/vmlinuz-raw",
                initrdPath: initrd ?? ".download/ubuntu/initramfs-containerd",
                useAgent: true,
                fresh: false,
                sharePath: sharePath,
                mountTag: mountTag,
                memoryGiB: memory,
                cpuCount: cpus,
                consoleOutputPath: stateDir.appendingPathComponent("console.log").path,
                containerdDiskPath: containerdDiskPath
            ),
            idleSeconds: idleSeconds,
            debug: debug
        )
    }

    fileprivate class Daemon: VMLifecycleManagerDelegate {
        let manager: VMLifecycleManager
        let idleSeconds: TimeInterval
        private let phaseTimer: BootPhaseTimer
        private var server: ControlServer?
        private var dockerProxyServer: DockerProxyServer?
        private var buildkitProxyServer: DockerProxyServer?
        private var portForwarder: PortForwarder?
        private var idleTimer: Timer?
        private var isShuttingDown = false
        private var restartAttempts = 0
        private var isRestartingVM = false
        /// When the VM last became ready; strike-based crash detection is
        /// ignored for a short grace window after readiness — the guest's
        /// Docker API server binds up to ~10 s after the agent control
        /// channel, and boot-time connect failures must not count as a crash.
        private var becameReadyAt: Date?
        /// Consecutive vsock-connect failures from the proxies. A guest kernel
        /// panic does NOT fire VZ's didStop delegate, so an unreachable guest
        /// (connect retries exhausted) is the crash signal for that case.
        private var guestUnreachableStrikes = 0
        private let cacheManager: ContainerdCacheManager?
        private var clientTracker: ClientTracker?

        init(manager: VMLifecycleManager, idleSeconds: TimeInterval, phaseTimer: BootPhaseTimer) {
            self.manager = manager
            self.idleSeconds = idleSeconds
            self.phaseTimer = phaseTimer
            self.cacheManager = ContainerdCacheManager(sharePath: manager.args.sharePath)
            manager.delegate = self
            manager.portCheckServer = PortCheckServer { [weak self] port in
                self?.portForwarder?.holdsTCP(port: port) ?? false
            }
        }

        /// Shared counter for control-socket and docker-socket clients. Both must
        /// keep the VM awake; previously only control clients were counted, so long
        /// docker compose / test runs could idle-timeout in the middle of work.
        private final class ClientTracker {
            private let lock = NSLock()
            private var count = 0
            private let idleSeconds: TimeInterval
            private let scheduleIdle: () -> Void
            private let cancelIdle: () -> Void
            /// When true, `disconnect()` will not re-schedule the idle timer.
            /// Set by the daemon during `idleTimeoutFired` to prevent the
            /// exec child processes (cache sync, page-cache drop) from
            /// triggering a new idle cycle via their connect/disconnect.
            var suppressIdleSchedule = false

            init(idleSeconds: TimeInterval, scheduleIdle: @escaping () -> Void, cancelIdle: @escaping () -> Void) {
                self.idleSeconds = idleSeconds
                self.scheduleIdle = scheduleIdle
                self.cancelIdle = cancelIdle
            }

            var isIdle: Bool {
                lock.lock(); defer { lock.unlock() }
                return count == 0
            }

            func connect() {
                var wasZero = false
                lock.lock()
                count += 1
                wasZero = count == 1
                lock.unlock()
                if wasZero { cancelIdle() }
            }

            func disconnect() {
                var isZero = false
                lock.lock()
                count -= 1
                isZero = count == 0
                lock.unlock()
                if isZero && !suppressIdleSchedule { scheduleIdle() }
            }
        }

        func start() {
            print("[anvil] daemon starting...")
            manager.start()
        }

        func shutdown() {
            guard !isShuttingDown else { return }
            isShuttingDown = true
            idleTimer?.invalidate()
            // Sync the containerd cache and drop guest page caches *before*
            // pausing/saving the VM. With containerd on a block disk the sync
            // is a no-op, but dropping caches shrinks the memory snapshot.
            // Run off the main queue so the run loop keeps the control server
            // alive for the guest exec calls.
            DispatchQueue.global().async { [weak self] in
                self?.cacheManager?.sync()
                GuestCacheDropper.dropCaches()
                DispatchQueue.main.async { [weak self] in
                    guard let self = self else { return }
                    self.server?.stop()
                    self.dockerProxyServer?.stop()
                    self.buildkitProxyServer?.stop()
                    self.portForwarder?.stop()
                    self.manager.stopAndSave {
                        releaseDaemonLock()
                        exit(0)
                    }
                }
            }
        }

        // MARK: - VMLifecycleManagerDelegate

        func vmLifecycleManagerDidBecomeReady(_ manager: VMLifecycleManager) {
            // The VM came back (first boot or crash restart). Backoff is NOT
            // reset here: on the restore path readiness fires right after
            // vm.resume, before the guest has done anything — resetting now
            // would make a panics-right-after-restore loop never reach the
            // fresh-boot escape. The counter is zeroed in handleVMCrash when
            // the VM had stayed up for a while.
            guestUnreachableStrikes = 0
            becameReadyAt = Date()

            // Host-port availability endpoint for the guest-agent is now
            // attached inside manager.start() before start/restore (its
            // listener must predate the guest dialing out).

            let tracker = ClientTracker(
                idleSeconds: idleSeconds,
                scheduleIdle: { [weak self] in self?.scheduleIdleTimer() },
                cancelIdle: { [weak self] in
                    self?.idleTimer?.invalidate()
                    self?.idleTimer = nil
                }
            )
            self.clientTracker = tracker

            let server = ControlServer(socketPath: controlSocketPath, deviceProvider: { [weak manager] in
                manager?.socketDevice
            }, debug: manager.args.debug)
            server.onClientConnect = { [weak self] in
                tracker.connect()
                self?.manager.ensureRunning { result in
                    if case .failure(let error) = result {
                        print("[anvil] resume on control client connect failed: \(error)")
                    }
                }
            }
            server.onClientDisconnect = {
                tracker.disconnect()
            }
            server.start()
            self.server = server
            phaseTimer.mark("control_sock")
            print("[anvil] daemon ready, control socket: \(controlSocketPath)")

            let dockerProxy = DockerProxyServer(
                socketPath: dockerSocketPath,
                deviceProvider: { [weak manager] in manager?.socketDevice },
                resumeProvider: { [weak manager] in
                    let sem = DispatchSemaphore(value: 0)
                    manager?.ensureRunning { result in
                        if case .failure(let error) = result {
                            print("[docker-proxy] resume request failed: \(error)")
                        }
                        sem.signal()
                    }
                    _ = sem.wait(timeout: .now() + .seconds(15))
                },
                debug: manager.args.debug
            )
            dockerProxy.onClientConnect = {
                tracker.connect()
            }
            dockerProxy.onClientDisconnect = {
                tracker.disconnect()
            }
            attachGuestLivenessHooks(dockerProxy)
            dockerProxy.start()
            self.dockerProxyServer = dockerProxy
            print("[anvil] docker proxy socket: \(dockerSocketPath)")

            // Buildkit bridge: buildx remote driver talks to this socket;
            // guest-agent forwards it to buildkitd (started lazily).
            let buildkitProxy = DockerProxyServer(
                socketPath: buildkitSocketPath,
                port: buildkitAPIPort,
                deviceProvider: { [weak manager] in manager?.socketDevice },
                resumeProvider: { [weak manager] in
                    let sem = DispatchSemaphore(value: 0)
                    manager?.ensureRunning { result in
                        if case .failure(let error) = result {
                            print("[buildkit-proxy] resume request failed: \(error)")
                        }
                        sem.signal()
                    }
                    _ = sem.wait(timeout: .now() + .seconds(15))
                },
                debug: manager.args.debug
            )
            buildkitProxy.onClientConnect = {
                tracker.connect()
            }
            buildkitProxy.onClientDisconnect = {
                tracker.disconnect()
            }
            attachGuestLivenessHooks(buildkitProxy)
            buildkitProxy.start()
            self.buildkitProxyServer = buildkitProxy
            print("[anvil] buildkit proxy socket: \(buildkitSocketPath)")

            let forwarder = PortForwarder { [weak manager] in manager?.socketDevice }
            forwarder.start()
            self.portForwarder = forwarder
            phaseTimer.mark("ready_binds")

            // Start the idle timer if no client connected while the VM was starting.
            if tracker.isIdle {
                scheduleIdleTimer()
            }
        }

        /// A proxy client could not reach the guest within its connect retry
        /// window. Two strikes in a row (no successful connect between them)
        /// treat the VM as crashed and restart it; a single exhausted window
        /// may also just be a wedged-but-recovering guest.
        private func attachGuestLivenessHooks(_ proxy: DockerProxyServer) {
            proxy.onVsockConnectFailure = { [weak self] in
                guard let self = self else { return }
                DispatchQueue.main.async {
                    guard !self.isShuttingDown, !self.isRestartingVM else { return }
                    if let ready = self.becameReadyAt, Date().timeIntervalSince(ready) < 20 {
                        return // still settling after boot
                    }
                    self.guestUnreachableStrikes += 1
                    print("[anvil] guest unreachable via proxy (\(self.guestUnreachableStrikes)/2)")
                    if self.guestUnreachableStrikes >= 2 {
                        self.handleVMCrash()
                    }
                }
            }
            proxy.onVsockConnectSuccess = { [weak self] in
                self?.guestUnreachableStrikes = 0
            }
        }

        func vmLifecycleManager(_ manager: VMLifecycleManager, didFailWithError error: Error) {
            print("[anvil] daemon VM failed: \(error)")
            handleVMCrash()
        }

        func vmLifecycleManagerDidStop(_ manager: VMLifecycleManager) {
            print("[anvil] daemon VM stopped")
            handleVMCrash()
        }

        // A crashed VM must not take the daemon (and the docker context) with
        // it: restart the VM with exponential backoff instead of exiting. A
        // stopped VM object is unusable, but VMLifecycleManager.start() builds
        // a fresh one and restores from the last snapshot.
        private func handleVMCrash() {
            DispatchQueue.main.async { [weak self] in
                guard let self = self else { return }
                guard !self.isShuttingDown, !self.isRestartingVM else { return }
                self.isRestartingVM = true
                self.guestUnreachableStrikes = 0
                self.idleTimer?.invalidate()

                // A VM that stayed up for a while before crashing is a NEW
                // incident, not part of a boot-crash loop: zero the attempt
                // counter so a healthy-then-crashed VM retries from the short
                // backoff (and never hits the fresh-boot discard).
                if let ready = self.becameReadyAt, Date().timeIntervalSince(ready) >= 60 {
                    self.restartAttempts = 0
                }

                // Drop the host-side bindings: they point at the dead VM's
                // socket device. Clients get EOF and retry; everything is
                // re-bound when the restarted VM becomes ready.
                self.server?.stop()
                self.dockerProxyServer?.stop()
                self.buildkitProxyServer?.stop()
                self.portForwarder?.stop()
                self.server = nil
                self.dockerProxyServer = nil
                self.buildkitProxyServer = nil
                self.portForwarder = nil
                self.clientTracker = nil

                let delay = daemonRestartDelay(attempt: self.restartAttempts)
                // Two crashes in a row restoring the same snapshot smells like
                // a poisoned snapshot (guest panics right after restore);
                // the third attempt discards it and cold-boots instead of
                // looping forever.
                let forceFresh = self.restartAttempts >= 2
                self.restartAttempts += 1
                print("[anvil] VM crashed; restarting in \(String(format: "%.0f", delay))s (attempt \(self.restartAttempts))\(forceFresh ? ", discarding snapshot" : "")")
                DispatchQueue.main.asyncAfter(deadline: .now() + delay) { [weak self] in
                    guard let self = self, !self.isShuttingDown else { return }
                    self.isRestartingVM = false
                    self.manager.start(fresh: forceFresh)
                }
            }
        }

        // MARK: - Idle handling

        private func scheduleIdleTimer() {
            DispatchQueue.main.async { [weak self] in
                guard let self = self else { return }
                self.idleTimer?.invalidate()
                self.idleTimer = Timer.scheduledTimer(withTimeInterval: self.idleSeconds, repeats: false) { [weak self] _ in
                    self?.idleTimeoutFired()
                }
            }
        }

        private func idleTimeoutFired() {
            guard !isShuttingDown, (server?.clientsCount ?? 0) == 0 else { return }
            clientTracker?.suppressIdleSchedule = true
            print("[anvil] idle timeout reached, syncing containerd cache...")
            cacheManager?.sync()
            GuestCacheDropper.dropCaches()
            print("[anvil] pausing VM...")
            manager.pause { [weak self] result in
                guard let self = self else { return }
                switch result {
                case .success:
                    print("[anvil] VM paused")
                    self.manager.saveSnapshot { [weak self] _ in
                        self?.clientTracker?.suppressIdleSchedule = false
                    }
                case .failure(let error):
                    print("[anvil] idle pause failed: \(error)")
                    self.clientTracker?.suppressIdleSchedule = false
                }
            }
        }
    }
}

private let daemonPIDFile = stateDir.appendingPathComponent("daemon.pid")

/// Exponential backoff for VM crash restarts: 1s doubling to a 60s cap.
/// Free function (not a Daemon method) so unit tests can exercise it.
func daemonRestartDelay(attempt: Int) -> TimeInterval {
    let delay = TimeInterval(1 << min(max(attempt, 0), 6))
    return min(delay, 60)
}
