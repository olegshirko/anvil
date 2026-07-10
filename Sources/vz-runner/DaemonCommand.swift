import Foundation
import Virtualization

fileprivate var globalDaemon: DaemonCommand.Daemon?

enum DaemonCommand {
    static func run(args: [String]) {
        setbuf(stdout, nil)
        let cliArgs = parseDaemonArgs(args)

        guard acquireDaemonLock() else {
            print("[vz-runner] daemon already running")
            exit(1)
        }

        let manager = VMLifecycleManager(args: cliArgs.bootArgs)
        let daemon = Daemon(manager: manager, idleSeconds: cliArgs.idleSeconds)
        globalDaemon = daemon

        signal(SIGINT) { _ in
            print("\n[vz-runner] daemon received SIGINT, shutting down...")
            globalDaemon?.shutdown()
        }

        signal(SIGTERM) { _ in
            print("\n[vz-runner] daemon received SIGTERM, shutting down...")
            globalDaemon?.shutdown()
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
        if let data = try? Data(contentsOf: pidFile),
           let s = String(data: data, encoding: .utf8),
           let pid = Int32(s.trimmingCharacters(in: .whitespacesAndNewlines)),
           kill(pid, 0) == 0 {
            return false
        }

        let pid = getpid()
        if let data = String(pid).data(using: .utf8) {
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
    }

    private static func parseDaemonArgs(_ raw: [String]) -> DaemonCLIArgs {
        var kernel: String?
        var initrd: String?
        var sharePath: String?
        var mountTag: String = "anvil"
        var cpus: Int = 2
        var memory: UInt64 = 2
        var idleSeconds: TimeInterval = 60

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
                sharePath: sharePath ?? "/tmp/anvil-share",
                mountTag: mountTag,
                memoryGiB: memory,
                cpuCount: cpus
            ),
            idleSeconds: idleSeconds
        )
    }

    fileprivate class Daemon: VMLifecycleManagerDelegate {
        let manager: VMLifecycleManager
        let idleSeconds: TimeInterval
        private var server: ControlServer?
        private var dockerProxyServer: DockerProxyServer?
        private var portForwarder: PortForwarder?
        private var idleTimer: Timer?
        private var isShuttingDown = false
        private let cacheManager: ContainerdCacheManager?

        init(manager: VMLifecycleManager, idleSeconds: TimeInterval) {
            self.manager = manager
            self.idleSeconds = idleSeconds
            self.cacheManager = ContainerdCacheManager(sharePath: manager.args.sharePath)
            manager.delegate = self
        }

        func start() {
            print("[vz-runner] daemon starting...")
            manager.start()
        }

        func shutdown() {
            guard !isShuttingDown else { return }
            isShuttingDown = true
            idleTimer?.invalidate()
            // Sync containerd cache while the control socket is still available.
            cacheManager?.sync()
            server?.stop()
            dockerProxyServer?.stop()
            portForwarder?.stop()
            manager.stopAndSave {
                releaseDaemonLock()
                exit(0)
            }
        }

        // MARK: - VMLifecycleManagerDelegate

        func vmLifecycleManagerDidBecomeReady(_ manager: VMLifecycleManager) {
            let server = ControlServer(socketPath: controlSocketPath) { [weak manager] in
                manager?.socketDevice
            }
            server.onClientConnect = { [weak self] in
                self?.clientDidConnect()
            }
            server.onClientDisconnect = { [weak self] in
                self?.clientDidDisconnect()
            }
            server.start()
            self.server = server
            print("[vz-runner] daemon ready, control socket: \(controlSocketPath)")

            let dockerProxy = DockerProxyServer(socketPath: dockerSocketPath) { [weak manager] in
                manager?.socketDevice
            }
            dockerProxy.start()
            self.dockerProxyServer = dockerProxy
            print("[vz-runner] docker proxy socket: \(dockerSocketPath)")

            let forwarder = PortForwarder { [weak manager] in manager?.socketDevice }
            forwarder.start()
            self.portForwarder = forwarder

            // Start the idle timer if no client connected while the VM was starting.
            if server.clientsCount == 0 {
                scheduleIdleTimer()
            }
        }

        func vmLifecycleManager(_ manager: VMLifecycleManager, didFailWithError error: Error) {
            print("[vz-runner] daemon VM failed: \(error)")
            shutdown()
        }

        func vmLifecycleManagerDidStop(_ manager: VMLifecycleManager) {
            print("[vz-runner] daemon VM stopped")
            shutdown()
        }

        // MARK: - Idle handling

        private func clientDidConnect() {
            DispatchQueue.main.async { [weak self] in
                self?.idleTimer?.invalidate()
                self?.idleTimer = nil
            }

            // If the VM is paused because of a previous idle timeout, resume it
            // before the request handler tries to open a vsock connection.
            let sem = DispatchSemaphore(value: 0)
            manager.ensureRunning { result in
                if case .failure(let error) = result {
                    print("[vz-runner] resume on client connect failed: \(error)")
                }
                sem.signal()
            }
            _ = sem.wait(timeout: .now() + .seconds(15))
        }

        private func clientDidDisconnect() {
            guard !isShuttingDown else { return }
            scheduleIdleTimer()
        }

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
            print("[vz-runner] idle timeout reached, syncing containerd cache...")
            cacheManager?.sync()
            print("[vz-runner] pausing VM...")
            manager.pause { [weak self] result in
                guard let self = self else { return }
                switch result {
                case .success:
                    print("[vz-runner] VM paused")
                    self.manager.saveSnapshot { _ in }
                case .failure(let error):
                    print("[vz-runner] idle pause failed: \(error)")
                }
            }
        }
    }
}

private let daemonPIDFile = stateDir.appendingPathComponent("daemon.pid")
