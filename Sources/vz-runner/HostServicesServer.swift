import Foundation
import Virtualization

// Host-listening ports for Docker Desktop's host services (guest side:
// guest-agent/hostservices.go).
let sshAgentPort: UInt32 = 1029
let hostLoopbackPort: UInt32 = 1030

/// Docker Desktop host services, reached by the guest over vsock:
///
/// - 1029: SSH agent forwarding. Every connection is relayed to the Mac's
///   ssh-agent ($SSH_AUTH_SOCK). The guest exposes it to containers as
///   /run/host-services/ssh-auth.sock.
/// - 1030: the Mac's localhost for host.docker.internal. The guest sends a
///   length-prefixed JSON {"port":N}; the host connects to 127.0.0.1:N (then
///   [::1]:N), answers {} or {"error":…}, and relays bytes. Unlike the egress
///   path (1028), loopback is the point here: this is what makes a service
///   bound to the Mac's 127.0.0.1 reachable from containers, as in Docker
///   Desktop. Only the VM can open vsock connections.
final class HostServicesServer: NSObject {
    private struct LoopbackRequest: Codable {
        let port: Int
    }

    private struct Reply: Codable {
        var error: String?
    }

    private var listeners: [VZVirtioSocketListener] = []
    private var delegates: [ConnectionDelegate] = []

    /// Install both listeners on the VM's socket device. Must be called
    /// before the VM starts (or before a snapshot restore).
    func attach(to device: VZVirtioSocketDevice) {
        listeners = []
        delegates = []
        install(on: device, port: sshAgentPort) { [weak self] in self?.handleSSHAgent($0) }
        install(on: device, port: hostLoopbackPort) { [weak self] in self?.handleLoopback($0) }
    }

    private func install(on device: VZVirtioSocketDevice, port: UInt32,
                         handler: @escaping (VZVirtioSocketConnection) -> Void) {
        let delegate = ConnectionDelegate(onConnection: handler)
        let listener = VZVirtioSocketListener()
        listener.delegate = delegate
        device.setSocketListener(listener, forPort: port)
        listeners.append(listener)
        delegates.append(delegate)
    }

    private func handleSSHAgent(_ connection: VZVirtioSocketConnection) {
        DispatchQueue.global(qos: .utility).async {
            defer { connection.close() }
            guard let path = sshAuthSocketPath() else {
                print("[host-services] ssh-agent: SSH_AUTH_SOCK is not set for the daemon")
                return
            }
            guard let agent = connectUnixSocket(path) else {
                print("[host-services] ssh-agent: cannot connect to \(path): \(String(cString: strerror(errno)))")
                return
            }
            defer { close(agent) }
            relayBothWays(connection.fileDescriptor, agent)
        }
    }

    private func handleLoopback(_ connection: VZVirtioSocketConnection) {
        DispatchQueue.global(qos: .utility).async {
            let vfd = connection.fileDescriptor
            defer { connection.close() }
            guard let request = try? decodeLengthPrefixedFD(LoopbackRequest.self, fd: vfd) else { return }
            let upstream: Int32
            switch dialHostLoopback(port: request.port, timeoutSeconds: 5) {
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

    private final class ConnectionDelegate: NSObject, VZVirtioSocketListenerDelegate {
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

/// Connect to a TCP port on the Mac's loopback: 127.0.0.1 first, then ::1.
func dialHostLoopback(port: Int, timeoutSeconds: Int) -> Result<Int32, EgressError> {
    guard (1...65535).contains(port) else {
        return .failure(EgressError(message: "invalid port \(port)"))
    }
    var v4 = sockaddr_in()
    v4.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
    v4.sin_family = sa_family_t(AF_INET)
    v4.sin_port = in_port_t(UInt16(port).bigEndian)
    v4.sin_addr.s_addr = in_addr_t(INADDR_LOOPBACK).bigEndian
    let first = withUnsafePointer(to: &v4) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            connectWithTimeout($0, socklen_t(MemoryLayout<sockaddr_in>.size), timeoutSeconds: timeoutSeconds)
        }
    }
    if case .success = first { return first }

    var v6 = sockaddr_in6()
    v6.sin6_len = UInt8(MemoryLayout<sockaddr_in6>.size)
    v6.sin6_family = sa_family_t(AF_INET6)
    v6.sin6_port = in_port_t(UInt16(port).bigEndian)
    v6.sin6_addr = in6addr_loopback
    let second = withUnsafePointer(to: &v6) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            connectWithTimeout($0, socklen_t(MemoryLayout<sockaddr_in6>.size), timeoutSeconds: timeoutSeconds)
        }
    }
    if case .success = second { return second }
    if case .failure(let error) = first {
        return .failure(EgressError(message: "connect localhost:\(port): \(error.message)"))
    }
    return second
}

/// The Mac's ssh-agent socket: the daemon's own SSH_AUTH_SOCK, else the
/// launchd session value (a daemon started by launchd may not inherit it).
func sshAuthSocketPath(environment: [String: String] = ProcessInfo.processInfo.environment) -> String? {
    if let path = environment["SSH_AUTH_SOCK"], !path.isEmpty {
        return path
    }
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: "/bin/launchctl")
    proc.arguments = ["getenv", "SSH_AUTH_SOCK"]
    let pipe = Pipe()
    proc.standardOutput = pipe
    proc.standardError = FileHandle.nullDevice
    guard (try? proc.run()) != nil else { return nil }
    proc.waitUntilExit()
    let out = String(data: pipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8)?
        .trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
    return out.isEmpty ? nil : out
}

/// Connect to a unix stream socket; nil (errno set) on failure.
func connectUnixSocket(_ path: String) -> Int32? {
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    guard path.utf8.count < MemoryLayout.size(ofValue: addr.sun_path) else {
        errno = ENAMETOOLONG
        return nil
    }
    _ = path.withCString {
        strncpy(&addr.sun_path.0, $0, MemoryLayout.size(ofValue: addr.sun_path) - 1)
    }
    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    guard fd >= 0 else { return nil }
    setSocketNoSigPipe(fd)
    let connected = withUnsafePointer(to: &addr) { ptr -> Bool in
        ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
        }
    }
    guard connected else {
        let saved = errno
        close(fd)
        errno = saved
        return nil
    }
    return fd
}
