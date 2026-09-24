import Foundation
import Virtualization

func setSocketNoSigPipe(_ fd: Int32) {
    var one: Int32 = 1
    setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
}

/// Classify a failed accept(2) on a listening socket: true when the socket is
/// gone (closed by stop()), false to retry. Descriptor exhaustion backs off
/// briefly — retrying it immediately spun a core at 100%.
func acceptShouldStop(_ err: Int32) -> Bool {
    switch err {
    case EBADF, EINVAL, ENOTSOCK:
        return true
    case EINTR, ECONNABORTED:
        return false
    default:
        Thread.sleep(forTimeInterval: 0.1)
        return false
    }
}

/// Unix-domain socket control server. Accepts length-prefixed JSON requests from
/// local clients and forwards the raw bytes to the guest agent over vsock.
final class ControlServer {
    private let socketPath: String
    private let deviceProvider: () -> VZVirtioSocketDevice?
    private var fd: Int32 = -1
    private let lock = NSLock()
    private var activeClients: Int = 0

    var onClientConnect: (() -> Void)?
    var onClientDisconnect: (() -> Void)?

    private let debug: Bool

    init(socketPath: String, deviceProvider: @escaping () -> VZVirtioSocketDevice?, debug: Bool = false) {
        self.socketPath = socketPath
        self.deviceProvider = deviceProvider
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
            print("[anvil] failed to create unix socket")
            return
        }
        setSocketNoSigPipe(fd)

        try? FileManager.default.removeItem(atPath: socketPath)

        let size = socklen_t(MemoryLayout<sockaddr_un>.size)
        let bindResult = withUnsafePointer(to: &addr) { ptr -> Int32 in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, size) }
        }
        guard bindResult == 0 else {
            print("[anvil] failed to bind unix socket: \(String(cString: strerror(errno)))")
            close(fd)
            fd = -1
            return
        }
        // The control socket executes arbitrary commands in the VM as root;
        // restrict it to the owner.
        chmod(socketPath, 0o600)
        guard listen(fd, 5) == 0 else {
            print("[anvil] failed to listen unix socket: \(String(cString: strerror(errno)))")
            close(fd)
            fd = -1
            return
        }

        let listenFD = fd
        DispatchQueue.global().async { [weak self] in
            while let self = self, self.isListening(on: listenFD) {
                let client = accept(listenFD, nil, nil)
                guard client >= 0 else {
                    if acceptShouldStop(errno) { break }
                    continue
                }
                setSocketNoSigPipe(client)
                DispatchQueue.global().async { [weak self] in
                    self?.handleClient(fd: client)
                }
            }
        }
    }

    private func isListening(on listenFD: Int32) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        return fd == listenFD
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

    /// Current number of connected clients.
    var clientsCount: Int {
        lock.lock(); defer { lock.unlock() }
        return activeClients
    }

    // MARK: - Private

    private func handleClient(fd clientFd: Int32) {
        incrementClients()
        defer {
            close(clientFd)
            decrementClients()
        }

        guard let device = deviceProvider() else {
            let data = (try? encodeLengthPrefixed(ControlResponse(stdout: nil, stderr: nil, exitCode: 1, error: "vm not ready", status: nil))) ?? Data()
            _ = data.withUnsafeBytes { write(clientFd, $0.baseAddress, $0.count) }
            return
        }

        let vsockConn = connectVsock(device: device, port: controlPort, until: Date().addingTimeInterval(20))
        guard let conn = vsockConn else {
            let data = (try? encodeLengthPrefixed(ControlResponse(stdout: nil, stderr: nil, exitCode: 1, error: "vsock connect failed", status: nil))) ?? Data()
            _ = data.withUnsafeBytes { write(clientFd, $0.baseAddress, $0.count) }
            return
        }
        defer { conn.close() }

        let vfd = conn.fileDescriptor

        guard let lengthData = readExactlyFD(clientFd, count: MemoryLayout<UInt32>.size) else {
            let data = (try? encodeLengthPrefixed(ControlResponse(stdout: nil, stderr: nil, exitCode: 1, error: "failed to read request length", status: nil))) ?? Data()
            _ = data.withUnsafeBytes { write(clientFd, $0.baseAddress, $0.count) }
            return
        }
        let length = UInt32(bigEndian: lengthData.withUnsafeBytes { $0.load(as: UInt32.self) })
        guard length <= 16 * 1024 * 1024,
              let body = readExactlyFD(clientFd, count: Int(length)) else {
            let data = (try? encodeLengthPrefixed(ControlResponse(stdout: nil, stderr: nil, exitCode: 1, error: "failed to read request body", status: nil))) ?? Data()
            _ = data.withUnsafeBytes { write(clientFd, $0.baseAddress, $0.count) }
            return
        }

        if debug,
           let req = try? JSONDecoder().decode(ControlRequest.self, from: body) {
            print("[control] command=\(req.cmd) args=\(req.args ?? [])")
        }

        do {
            try writeExactlyFD(vfd, data: lengthData + body)
            let resp = try decodeLengthPrefixedFD(ControlResponse.self, fd: vfd)
            if debug {
                print("[control] response exit=\(resp.exitCode ?? -1) error=\(resp.error ?? "")")
            }
            let data = try encodeLengthPrefixed(resp)
            _ = data.withUnsafeBytes { write(clientFd, $0.baseAddress, $0.count) }
        } catch {
            let data = (try? encodeLengthPrefixed(ControlResponse(stdout: nil, stderr: nil, exitCode: 1, error: error.localizedDescription, status: nil))) ?? Data()
            _ = data.withUnsafeBytes { write(clientFd, $0.baseAddress, $0.count) }
        }
    }

    private func incrementClients() {
        lock.lock()
        activeClients += 1
        lock.unlock()
        onClientConnect?()
    }

    private func decrementClients() {
        lock.lock()
        activeClients -= 1
        lock.unlock()
        onClientDisconnect?()
    }
}
