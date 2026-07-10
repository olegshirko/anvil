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
    private let deviceProvider: () -> VZVirtioSocketDevice?
    private var fd: Int32 = -1
    private let lock = NSLock()

    init(socketPath: String, deviceProvider: @escaping () -> VZVirtioSocketDevice?) {
        self.socketPath = socketPath
        self.deviceProvider = deviceProvider
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
        defer { close(clientFd) }

        guard let device = deviceProvider() else {
            print("[docker-proxy] vm not ready, closing client")
            return
        }

        let deadline = Date().addingTimeInterval(20)
        var vsockConn: VZVirtioSocketConnection?
        while vsockConn == nil, Date() < deadline {
            let sem = DispatchSemaphore(value: 0)
            DispatchQueue.main.async {
                device.connect(toPort: dockerAPIPort) { result in
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
        defer { conn.close() }

        let vfd = conn.fileDescriptor
        let group = DispatchGroup()

        // Host client -> guest Docker API.
        group.enter()
        DispatchQueue.global().async {
            var buf = [UInt8](repeating: 0, count: 65536)
            while true {
                let n = read(clientFd, &buf, buf.count)
                if n <= 0 { break }
                let written = buf.withUnsafeBytes { write(vfd, $0.baseAddress, n) }
                if written < 0 { break }
            }
            _ = shutdown(vfd, Int32(SHUT_WR))
            group.leave()
        }

        // Guest Docker API -> host client.
        group.enter()
        DispatchQueue.global().async {
            var buf = [UInt8](repeating: 0, count: 65536)
            while true {
                let n = read(vfd, &buf, buf.count)
                if n <= 0 { break }
                let written = buf.withUnsafeBytes { write(clientFd, $0.baseAddress, n) }
                if written < 0 { break }
            }
            _ = shutdown(clientFd, Int32(SHUT_WR))
            group.leave()
        }

        group.wait()
    }
}
