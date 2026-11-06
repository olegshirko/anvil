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
        // SOMAXCONN: compose opens a connection per service at once; with a
        // backlog of 5 the excess got ECONNREFUSED ("Cannot connect to the
        // Docker daemon") mid-`compose up`.
        guard listen(fd, SOMAXCONN) == 0 else {
            print("[anvil] failed to listen unix socket: \(String(cString: strerror(errno)))")
            close(fd)
            fd = -1
            return
        }

        // Dedicated threads, not GCD's shared pool: every client blocks on
        // vsock and guest I/O (an `anvil exec` for as long as its command
        // runs), and the shutdown and idle paths need that pool themselves.
        let listenFD = fd
        startLoopThread(name: "control-accept") { [weak self] in
            while let self = self, self.isListening(on: listenFD) {
                let client = accept(listenFD, nil, nil)
                guard client >= 0 else {
                    if acceptShouldStop(errno) { break }
                    continue
                }
                setSocketNoSigPipe(client)
                runOnConnectionThread(name: "control-client", limiter: hostConnectionLimiter,
                                      onReject: { close(client) }) { [weak self] in
                    guard let self = self else {
                        close(client)
                        return
                    }
                    self.handleClient(fd: client)
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
        let wasListening = fd >= 0
        if wasListening {
            close(fd)
            fd = -1
        }
        lock.unlock()
        // Only while this instance owns the listener: a stopped server that
        // outlives a restart (its client threads still hold it) must not
        // delete the new server's socket (see DockerProxyServer.closeListener).
        if wasListening {
            try? FileManager.default.removeItem(atPath: socketPath)
        }
    }

    /// Current number of connected clients.
    var clientsCount: Int {
        lock.lock(); defer { lock.unlock() }
        return activeClients
    }

    // MARK: - Private

    private func handleClient(fd clientFd: Int32) {
        // Read the request before counting the connection as a client: the
        // daemon's own NAT egress probe arrives every minute, and counting it
        // re-armed the idle timer, so --idle never fired.
        guard let lengthData = readExactlyFD(clientFd, count: MemoryLayout<UInt32>.size) else {
            writeControlError(clientFd, "failed to read request length")
            close(clientFd)
            return
        }
        let length = UInt32(bigEndian: lengthData.withUnsafeBytes { $0.load(as: UInt32.self) })
        guard length <= 16 * 1024 * 1024,
              let body = readExactlyFD(clientFd, count: Int(length)) else {
            writeControlError(clientFd, "failed to read request body")
            close(clientFd)
            return
        }

        let request = try? JSONDecoder().decode(ControlRequest.self, from: body)
        let internalRequest = request?.internalRequest ?? false
        if !internalRequest {
            incrementClients()
        }
        defer {
            close(clientFd)
            if !internalRequest {
                decrementClients()
            }
        }

        if debug, let req = request {
            print("[control] command=\(req.cmd) args=\(req.args ?? [])")
        }

        // Bounded I/O on the client socket: a client that connects and
        // stalls (half a header, an unread response) used to park one of
        // the dedicated connection threads forever. SO_RCVTIMEO makes the
        // reads fail, SO_SNDTIMEO keeps writeExactlyFD from hanging on a
        // client that stopped reading.
        var tv = timeval(tv_sec: 30, tv_usec: 0)
        setsockopt(clientFd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        setsockopt(clientFd, SOL_SOCKET, SO_SNDTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))

        // A VM stopped to free memory is being restored (the connect hook
        // asked for it): wait for its socket device, bounded.
        var device = deviceProvider()
        let deviceDeadline = Date().addingTimeInterval(20)
        while device == nil && Date() < deviceDeadline {
            Thread.sleep(forTimeInterval: 0.1)
            device = deviceProvider()
        }
        guard let device = device else {
            writeControlError(clientFd, "vm not ready")
            return
        }

        let vsockConn = connectVsock(device: device, port: controlPort, until: Date().addingTimeInterval(20))
        guard let conn = vsockConn else {
            writeControlError(clientFd, "vsock connect failed")
            return
        }
        defer { conn.close() }

        let vfd = conn.fileDescriptor

        do {
            try writeExactlyFD(vfd, data: lengthData + body)
            let resp = try decodeLengthPrefixedFD(ControlResponse.self, fd: vfd)
            if debug {
                print("[control] response exit=\(resp.exitCode ?? -1) error=\(resp.error ?? "")")
            }
            // writeExactlyFD, not a single write(): a response larger than
            // the client's socket buffer was silently truncated mid-stream.
            let data = try encodeLengthPrefixed(resp)
            try writeExactlyFD(clientFd, data: data)
        } catch {
            writeControlError(clientFd, error.localizedDescription)
        }
    }

    /// Sends a length-prefixed error response, fully: a partial write left
    /// the client waiting on a truncated body.
    private func writeControlError(_ fd: Int32, _ message: String) {
        let data = (try? encodeLengthPrefixed(ControlResponse(stdout: nil, stderr: nil, exitCode: 1, error: message, status: nil))) ?? Data()
        try? writeExactlyFD(fd, data: data)
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
