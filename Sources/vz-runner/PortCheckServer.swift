import Foundation
import Virtualization

/// Host-side vsock listener (port 1027) that answers guest-agent queries
/// about localhost TCP ports. The guest dials out to the host (CID 2) before
/// creating a container with published ports; ports already bound by a
/// foreign process (Docker Desktop, Lima, a local postgres) are reported as
/// busy so container creation fails with a clear Docker-style error instead
/// of starting a container that is silently unreachable on localhost.
///
/// Ports held by anvil's own PortForwarder are NOT reported: conflicts
/// between anvil containers are refused separately, and reporting them here
/// would break `compose up --force-recreate` (the old container's listener
/// may still be unbinding while the replacement is being created).
final class PortCheckServer: NSObject {
    private struct CheckRequest: Codable {
        let ports: [Int]
    }

    private struct CheckResponse: Codable {
        let busy: [Int]
    }

    private let holdsTCP: (Int) -> Bool
    private var listener: VZVirtioSocketListener?
    private var delegate: Delegate?

    /// `holdsTCP` reports whether a host port is bound by anvil's own
    /// port forwarder.
    init(holdsTCP: @escaping (Int) -> Bool) {
        self.holdsTCP = holdsTCP
    }

    /// Install the listener on the VM's socket device. Must be called before
    /// the VM starts (or before a snapshot restore).
    func attach(to device: VZVirtioSocketDevice) {
        let delegate = Delegate { [weak self] connection in
            self?.handle(connection: connection)
        }
        let listener = VZVirtioSocketListener()
        listener.delegate = delegate
        device.setSocketListener(listener, forPort: portCheckPort)
        self.listener = listener
        self.delegate = delegate
    }

    private func handle(connection: VZVirtioSocketConnection) {
        DispatchQueue.global(qos: .utility).async { [self] in
            let fd = connection.fileDescriptor
            defer { connection.close() }
            guard let request = try? decodeLengthPrefixedFD(CheckRequest.self, fd: fd) else { return }
            var busy: [Int] = []
            for port in request.ports where port > 0 && port <= 65535 {
                if holdsTCP(port) { continue }
                if !canBind(port: port) { busy.append(port) }
            }
            guard let data = try? encodeLengthPrefixed(CheckResponse(busy: busy)) else { return }
            try? writeExactlyFD(fd, data: data)
        }
    }

    /// Probe-bind a port the way the PortForwarder listener does, then
    /// release it. Both address families are probed: a dual-stack IPv6
    /// wildcard bind does not conflict with an IPv4-only wildcard squatter
    /// on macOS, yet the squatter still captures 127.0.0.1 traffic.
    private func canBind(port: Int) -> Bool {
        canBind_INET6(port: port) && canBind_INET(port: port)
    }

    private func canBind_INET6(port: Int) -> Bool {
        let fd = socket(AF_INET6, SOCK_STREAM, 0)
        guard fd >= 0 else { return false }
        defer { close(fd) }

        var reuse: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &reuse, socklen_t(MemoryLayout<Int32>.size))
        var off: Int32 = 0
        setsockopt(fd, IPPROTO_IPV6, IPV6_V6ONLY, &off, socklen_t(MemoryLayout<Int32>.size))

        var addr = sockaddr_in6()
        addr.sin6_family = sa_family_t(AF_INET6)
        addr.sin6_port = in_port_t(port).bigEndian
        addr.sin6_addr = in6addr_any

        let bindResult = withUnsafePointer(to: &addr) { ptr -> Int32 in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in6>.size))
            }
        }
        guard bindResult == 0, listen(fd, 1) == 0 else { return false }
        return true
    }

    private func canBind_INET(port: Int) -> Bool {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { return false }
        defer { close(fd) }

        var reuse: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &reuse, socklen_t(MemoryLayout<Int32>.size))

        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = in_port_t(port).bigEndian
        addr.sin_addr = in_addr(s_addr: INADDR_ANY)

        let bindResult = withUnsafePointer(to: &addr) { ptr -> Int32 in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
            }
        }
        guard bindResult == 0, listen(fd, 1) == 0 else { return false }
        return true
    }

    /// VZVirtioSocketListenerDelegate is an NSObjectProtocol; isolated here
    /// so the accept path is a plain closure.
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
