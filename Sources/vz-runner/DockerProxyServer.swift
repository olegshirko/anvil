import Foundation
import Virtualization

/// Host-side proxy that exposes a Docker-compatible Unix socket at
/// `~/.anvil-vz/docker.sock` and forwards raw HTTP byte streams to the
/// guest-agent's Docker API server over a dedicated vsock port.
///
/// This is intentionally a dumb pipe: no HTTP parsing happens on the host.
/// The guest-agent implements the actual Docker REST endpoints.
final class DockerProxyServer {
    private let socketPath: String
    private let port: UInt32
    private let deviceProvider: () -> VZVirtioSocketDevice?
    private let resumeProvider: () -> Void
    private let debug: Bool
    private var fd: Int32 = -1
    private let lock = NSLock()

    var onClientConnect: (() -> Void)?
    var onClientDisconnect: (() -> Void)?

    init(socketPath: String, port: UInt32 = dockerAPIPort, deviceProvider: @escaping () -> VZVirtioSocketDevice?, resumeProvider: @escaping () -> Void = {}, debug: Bool = false) {
        self.socketPath = socketPath
        self.port = port
        self.deviceProvider = deviceProvider
        self.resumeProvider = resumeProvider
        self.debug = debug
    }

    deinit {
        stop()
    }

    /// Bind the unix socket and start accepting connections on a background queue.
    func start() {
        stop()

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        _ = socketPath.withCString {
            strncpy(&addr.sun_path.0, $0, MemoryLayout.size(ofValue: addr.sun_path) - 1)
        }

        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            print("[docker-proxy] failed to create unix socket")
            return
        }
        setSocketNoSigPipe(fd)

        try? FileManager.default.removeItem(atPath: socketPath)

        let size = socklen_t(MemoryLayout<sockaddr_un>.size)
        let bindResult = withUnsafePointer(to: &addr) { ptr -> Int32 in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, size) }
        }
        guard bindResult == 0 else {
            print("[docker-proxy] failed to bind unix socket: \(String(cString: strerror(errno)))")
            close(fd)
            fd = -1
            return
        }
        // The Docker socket is root-equivalent inside the VM; restrict it to
        // the owner.
        chmod(socketPath, 0o600)
        guard listen(fd, 5) == 0 else {
            print("[docker-proxy] failed to listen unix socket: \(String(cString: strerror(errno)))")
            close(fd)
            fd = -1
            return
        }

        DispatchQueue.global().async { [weak self] in
            while let self = self, self.fd >= 0 {
                let client = accept(self.fd, nil, nil)
                guard client >= 0 else { continue }
                setSocketNoSigPipe(client)
                DispatchQueue.global().async { [weak self] in
                    self?.handleClient(fd: client)
                }
            }
        }
    }

    /// Close the listening socket and remove the path.
    func stop() {
        lock.lock()
        if fd >= 0 {
            close(fd)
            fd = -1
        }
        lock.unlock()
        try? FileManager.default.removeItem(atPath: socketPath)
    }

    // MARK: - Private

    private func handleClient(fd clientFd: Int32) {
        DispatchQueue.main.async { [weak self] in
            self?.onClientConnect?()
        }
        defer {
            close(clientFd)
            DispatchQueue.main.async { [weak self] in
                self?.onClientDisconnect?()
            }
        }

        // Ask the daemon to resume the VM if it is paused. The ControlServer
        // does this for its own clients, but docker.sock clients bypass it.
        resumeProvider()
        if debug {
            print("[docker-proxy] client connected")
        }

        guard let device = deviceProvider() else {
            print("[docker-proxy] vm not ready, closing client")
            return
        }

        let deadline = Date().addingTimeInterval(20)
        var vsockConn: VZVirtioSocketConnection?
        while vsockConn == nil, Date() < deadline {
            let sem = DispatchSemaphore(value: 0)
            DispatchQueue.main.async {
                device.connect(toPort: self.port) { result in
                    if case .success(let conn) = result {
                        vsockConn = conn
                    }
                    sem.signal()
                }
            }
            _ = sem.wait(timeout: .now() + .seconds(2))
            if vsockConn == nil {
                Thread.sleep(forTimeInterval: 0.2)
            }
        }
        guard let conn = vsockConn else {
            print("[docker-proxy] vsock connect failed, closing client")
            return
        }
        if debug {
            print("[docker-proxy] vsock connected, proxying")
        }
        defer {
            conn.close()
            if debug {
                print("[docker-proxy] client disconnected")
            }
        }

        let vfd = conn.fileDescriptor
        let group = DispatchGroup()

        // Host client -> guest Docker API.
        group.enter()
        DispatchQueue.global().async {
            var buf = [UInt8](repeating: 0, count: 65536)
            var total: Int = 0
            while true {
                let n = read(clientFd, &buf, buf.count)
                if n <= 0 { break }
                total += n
                if !self.writeAll(fd: vfd, buffer: buf, count: n) { break }
            }
            if self.debug {
                print("[docker-proxy] host -> guest: \(total) bytes")
            }
            _ = shutdown(vfd, Int32(SHUT_WR))
            group.leave()
        }

        // Guest Docker API -> host client.
        group.enter()
        DispatchQueue.global().async {
            var buf = [UInt8](repeating: 0, count: 65536)
            var total: Int = 0
            while true {
                let n = read(vfd, &buf, buf.count)
                if n <= 0 { break }
                total += n
                if !self.writeAll(fd: clientFd, buffer: buf, count: n) { break }
            }
            if self.debug {
                print("[docker-proxy] guest -> host: \(total) bytes")
            }
            _ = shutdown(clientFd, Int32(SHUT_WR))
            group.leave()
        }

        group.wait()
    }

    /// Write the whole buffer, looping over short writes. Blocking vsock and
    /// unix-socket fds may return fewer bytes than requested under memory
    /// pressure; dropping the remainder silently corrupts the proxied stream
    /// (e.g. truncated `docker load` request bodies).
    private func writeAll(fd: Int32, buffer: [UInt8], count: Int) -> Bool {
        var total = 0
        while total < count {
            let n = buffer.withUnsafeBytes { raw -> Int in
                guard let base = raw.baseAddress else { return -1 }
                return write(fd, base.advanced(by: total), count - total)
            }
            if n < 0 {
                if errno == EINTR { continue }
                return false
            }
            if n == 0 { return false }
            total += n
        }
        return true
    }
}
