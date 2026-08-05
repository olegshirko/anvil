import Foundation
import Virtualization

fileprivate var globalDaemon: DaemonCommand.Daemon?

enum DaemonCommand {
    static func run(args: [String]) {
        setbuf(stdout, nil)
        let cliArgs = parseDaemonArgs(args)

        guard acquireDaemonLock() else {
            print("[anvil] daemon already running")
            exit(1)
        }

        let manager = VMLifecycleManager(args: cliArgs.bootArgs)
        let daemon = Daemon(manager: manager, idleSeconds: cliArgs.idleSeconds)
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
                kernelPath: kernel ?? ".download/ubuntu/vmlinuz-raw",
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
        private var server: ControlServer?
        private var dockerProxyServer: DockerProxyServer?
        private var buildkitProxyServer: DockerProxyServer?
        private var portForwarder: PortForwarder?
        private var idleTimer: Timer?
        private var isShuttingDown = false
        private let cacheManager: ContainerdCacheManager?
        private var clientTracker: ClientTracker?

        init(manager: VMLifecycleManager, idleSeconds: TimeInterval) {
            self.manager = manager
            self.idleSeconds = idleSeconds
            self.cacheManager = ContainerdCacheManager(sharePath: manager.args.sharePath)
            manager.delegate = self
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
            buildkitProxy.start()
            self.buildkitProxyServer = buildkitProxy
            print("[anvil] buildkit proxy socket: \(buildkitSocketPath)")

            let forwarder = PortForwarder { [weak manager] in manager?.socketDevice }
            forwarder.start()
            self.portForwarder = forwarder

            // Start the idle timer if no client connected while the VM was starting.
            if tracker.isIdle {
                scheduleIdleTimer()
            }
        }

        func vmLifecycleManager(_ manager: VMLifecycleManager, didFailWithError error: Error) {
            print("[anvil] daemon VM failed: \(error)")
            shutdown()
        }

        func vmLifecycleManagerDidStop(_ manager: VMLifecycleManager) {
            print("[anvil] daemon VM stopped")
            shutdown()
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
