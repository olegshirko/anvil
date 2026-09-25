import Foundation
import Virtualization

// Host-listening port: the guest dials it to reach the internet through the
// Mac's network stack (see guest-agent/egressproxy.go).
let egressPort: UInt32 = 1028

/// Host-side vsock listener (port 1028) that opens outbound TCP connections
/// on behalf of the guest. The VM normally reaches the internet through the
/// macOS NAT; a full-tunnel VPN on the Mac (a Tailscale exit node) can drop
/// that NATed traffic while the Mac itself stays online. Connections made
/// here come from vz-runner, a regular host process, so they follow the
/// Mac's routing — VPN included. The guest-agent uses it only after a direct
/// connect failed, for registry-style traffic (pulls, pushes, logins,
/// buildkitd's HTTPS_PROXY).
///
/// Protocol: the guest sends a length-prefixed JSON {"target":"host:port"};
/// the host answers {} once connected (or {"error":…}) and then relays bytes
/// both ways until either side closes.
final class EgressServer: NSObject {
    private struct Request: Codable {
        let target: String
    }

    private struct Reply: Codable {
        var error: String?
    }

    private var listener: VZVirtioSocketListener?
    private var delegate: Delegate?

    /// Install the listener on the VM's socket device. Must be called before
    /// the VM starts (or before a snapshot restore).
    func attach(to device: VZVirtioSocketDevice) {
        let delegate = Delegate { [weak self] connection in
            self?.handle(connection: connection)
        }
        let listener = VZVirtioSocketListener()
        listener.delegate = delegate
        device.setSocketListener(listener, forPort: egressPort)
        self.listener = listener
        self.delegate = delegate
    }

    private func handle(connection: VZVirtioSocketConnection) {
        DispatchQueue.global(qos: .utility).async {
            let vfd = connection.fileDescriptor
            defer { connection.close() }
            guard let request = try? decodeLengthPrefixedFD(Request.self, fd: vfd) else { return }
            let upstream: Int32
            switch dialEgressTarget(request.target, timeoutSeconds: 10) {
            case .failure(let error):
                if let data = try? encodeLengthPrefixed(Reply(error: error.message)) {
                    try? writeExactlyFD(vfd, data: data)
                }
                return
            case .success(let fd):
                upstream = fd
            }
            defer { close(upstream) }
            guard let data = try? encodeLengthPrefixed(Reply()),
                  (try? writeExactlyFD(vfd, data: data)) != nil else { return }
            relayBothWays(vfd, upstream)
        }
    }

    private final class Delegate: NSObject, VZVirtioSocketListenerDelegate {
        private let onConnection: (VZVirtioSocketConnection) -> Void

        init(onConnection: @escaping (VZVirtioSocketConnection) -> Void) {
            self.onConnection = onConnection
        }

        func listener(_ listener: VZVirtioSocketListener,
                      shouldAcceptNewConnection connection: VZVirtioSocketConnection,
                      from socketDevice: VZVirtioSocketDevice) -> Bool {
            onConnection(connection)
            return true
        }
    }
}

struct EgressError: Error {
    let message: String
}

/// Split "host:port" / "[v6]:port" into its parts.
func parseEgressTarget(_ target: String) -> (host: String, port: Int)? {
    guard let colon = target.lastIndex(of: ":") else { return nil }
    var host = String(target[..<colon])
    guard let port = Int(target[target.index(after: colon)...]), (1...65535).contains(port) else { return nil }
    if host.hasPrefix("[") && host.hasSuffix("]") {
        host = String(host.dropFirst().dropLast())
    }
    guard !host.isEmpty else { return nil }
    return (host, port)
}

/// Loopback and unspecified addresses are refused: the guest must not reach
/// services bound to the Mac's localhost through this path.
func isForbiddenEgressAddress(_ addr: UnsafePointer<sockaddr>) -> Bool {
    switch Int32(addr.pointee.sa_family) {
    case AF_INET:
        return addr.withMemoryRebound(to: sockaddr_in.self, capacity: 1) {
            let a = UInt32(bigEndian: $0.pointee.sin_addr.s_addr)
            return a >> 24 == 127 || a == 0
        }
    case AF_INET6:
        return addr.withMemoryRebound(to: sockaddr_in6.self, capacity: 1) {
            let b = withUnsafeBytes(of: $0.pointee.sin6_addr) { Array($0) }
            let loopback = b[0..<15].allSatisfy { $0 == 0 } && b[15] == 1
            let unspecified = b.allSatisfy { $0 == 0 }
            let mapped = b[0..<10].allSatisfy { $0 == 0 } && b[10] == 0xff && b[11] == 0xff
            let mappedForbidden = mapped && (b[12] == 127 || b[12...15].allSatisfy { $0 == 0 })
            return loopback || unspecified || mappedForbidden
        }
    default:
        return true
    }
}

/// Resolve and connect with a timeout, trying each address in turn.
func dialEgressTarget(_ target: String, timeoutSeconds: Int) -> Result<Int32, EgressError> {
    guard let (host, port) = parseEgressTarget(target) else {
        return .failure(EgressError(message: "invalid target \(target)"))
    }
    var hints = addrinfo()
    hints.ai_family = AF_UNSPEC
    hints.ai_socktype = SOCK_STREAM
    var res: UnsafeMutablePointer<addrinfo>?
    let gai = getaddrinfo(host, String(port), &hints, &res)
    guard gai == 0, let first = res else {
        return .failure(EgressError(message: "resolve \(host): \(String(cString: gai_strerror(gai)))"))
    }
    defer { freeaddrinfo(first) }

    var lastError = "no address for \(host)"
    var cursor: UnsafeMutablePointer<addrinfo>? = first
    while let ai = cursor {
        cursor = ai.pointee.ai_next
        guard let addr = ai.pointee.ai_addr else { continue }
        if isForbiddenEgressAddress(addr) {
            lastError = "refusing loopback target \(target)"
            continue
        }
        switch connectWithTimeout(addr, ai.pointee.ai_addrlen, timeoutSeconds: timeoutSeconds) {
        case .success(let fd):
            return .success(fd)
        case .failure(let error):
            lastError = "connect \(target): \(error.message)"
        }
    }
    return .failure(EgressError(message: lastError))
}

/// Non-blocking TCP connect bounded by a timeout; the returned socket is
/// back in blocking mode.
func connectWithTimeout(_ addr: UnsafePointer<sockaddr>, _ len: socklen_t, timeoutSeconds: Int) -> Result<Int32, EgressError> {
    let fd = socket(Int32(addr.pointee.sa_family), SOCK_STREAM, 0)
    guard fd >= 0 else { return .failure(EgressError(message: String(cString: strerror(errno)))) }
    setSocketNoSigPipe(fd)
    let flags = fcntl(fd, F_GETFL)
    _ = fcntl(fd, F_SETFL, flags | O_NONBLOCK)
    var rc = connect(fd, addr, len)
    if rc != 0 && errno == EINPROGRESS {
        var pfd = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
        let ready = poll(&pfd, 1, Int32(timeoutSeconds * 1000))
        if ready == 1 {
            var soErr: Int32 = 0
            var optLen = socklen_t(MemoryLayout<Int32>.size)
            getsockopt(fd, SOL_SOCKET, SO_ERROR, &soErr, &optLen)
            rc = soErr == 0 ? 0 : -1
            if soErr != 0 { errno = soErr }
        } else {
            rc = -1
            errno = ETIMEDOUT
        }
    }
    if rc == 0 {
        _ = fcntl(fd, F_SETFL, flags)
        return .success(fd)
    }
    let message = String(cString: strerror(errno))
    close(fd)
    return .failure(EgressError(message: message))
}

/// Copy bytes in both directions until both sides are done, half-closing
/// the peer when one direction ends.
func relayBothWays(_ a: Int32, _ b: Int32) {
    let group = DispatchGroup()
    for (from, to) in [(a, b), (b, a)] {
        group.enter()
        DispatchQueue.global(qos: .utility).async {
            var buf = [UInt8](repeating: 0, count: 65536)
            outer: while true {
                let n = read(from, &buf, buf.count)
                if n < 0 && errno == EINTR { continue }
                if n <= 0 { break }
                var off = 0
                while off < n {
                    let w = buf.withUnsafeBytes { write(to, $0.baseAddress!.advanced(by: off), n - off) }
                    if w < 0 && errno == EINTR { continue }
                    if w <= 0 { break outer }
                    off += w
                }
            }
            _ = shutdown(to, Int32(SHUT_WR))
            group.leave()
        }
    }
    group.wait()
}
