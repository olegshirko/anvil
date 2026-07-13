import Foundation
import Virtualization
import Darwin

struct PortMapping: Codable, Hashable {
    let namespace: String
    let containerID: String
    let name: String?
    let hostPort: Int
    let containerPort: Int
    let `protocol`: String?
    let guestIP: String

    enum CodingKeys: String, CodingKey {
        case namespace
        case containerID = "container_id"
        case name
        case hostPort = "host_port"
        case containerPort = "container_port"
        case `protocol`
        case guestIP = "guest_ip"
    }

    /// Listener identity key. A single host-side listener is bound per exposed host port.
    var listenerKey: String {
        let proto = `protocol` ?? "tcp"
        return "\(namespace)/\(containerID)/\(hostPort)/\(proto)"
    }
}

struct PortMapState: Codable {
    let mappings: [PortMapping]
}

/// Subscribes to guest-agent port-mapping pushes and exposes localhost TCP listeners
/// that forward into the guest via vzNAT. This is the data plane; guest-agent is the
/// control plane.
final class PortForwarder {
    private let deviceProvider: () -> VZVirtioSocketDevice?
    private let queue = DispatchQueue(label: "com.olegshirko.anvil.port-forwarder", qos: .utility)

    private var listeners: [String: Listener] = [:]
    private let listenersLock = NSLock()

    private var isRunning = false

    init(deviceProvider: @escaping () -> VZVirtioSocketDevice?) {
        self.deviceProvider = deviceProvider
    }

    func start() {
        queue.async { [weak self] in
            guard let self = self else { return }
            self.isRunning = true
            self.runLoop()
        }
    }

    func stop() {
        queue.async { [weak self] in
            guard let self = self else { return }
            self.isRunning = false
            self.apply(state: PortMapState(mappings: []))
        }
    }

    // MARK: - Subscription loop

    private func runLoop() {
        while isRunning {
            guard let connection = connectToGuestAgent() else {
                Thread.sleep(forTimeInterval: 1.0)
                continue
            }
            let fd = connection.fileDescriptor

            // Send subscribe_ports request.
            let request = ControlRequest(cmd: "subscribe_ports", args: nil)
            guard let requestData = try? encodeLengthPrefixed(request),
                  writeAllFD(fd, data: requestData) else {
                connection.close()
                Thread.sleep(forTimeInterval: 1.0)
                continue
            }

            // Read full-state pushes.
            while isRunning {
                do {
                    let state = try decodeLengthPrefixedFD(PortMapState.self, fd: fd)
                    self.apply(state: state)
                } catch {
                    print("[port-forwarder] subscription read failed: \(error)")
                    break
                }
            }

            connection.close()
            // Clear stale listeners while disconnected; guest-agent will send a full state on reconnect.
            self.apply(state: PortMapState(mappings: []))
            Thread.sleep(forTimeInterval: 1.0)
        }
    }

    private func connectToGuestAgent() -> VZVirtioSocketConnection? {
        guard let device = deviceProvider() else {
            print("[port-forwarder] VM socket device not ready")
            return nil
        }

        var connection: VZVirtioSocketConnection?
        let sem = DispatchSemaphore(value: 0)
        DispatchQueue.main.async {
            device.connect(toPort: controlPort) { result in
                if case .success(let conn) = result {
                    connection = conn
                }
                sem.signal()
            }
        }
        _ = sem.wait(timeout: .now() + .seconds(5))
        return connection
    }

    // MARK: - Listener management

    private func apply(state: PortMapState) {
        listenersLock.lock()
        let currentKeys = Set(listeners.keys)
        let desiredKeys = Set(state.mappings.map { $0.listenerKey })
        listenersLock.unlock()

        let toRemove = currentKeys.subtracting(desiredKeys)
        let toAdd = state.mappings.filter { !currentKeys.contains($0.listenerKey) }

        for key in toRemove {
            stopListener(key: key)
        }
        for mapping in toAdd {
            startListener(mapping: mapping)
        }

        if !toRemove.isEmpty || !toAdd.isEmpty {
            print("[port-forwarder] active listeners: \(desiredKeys.count)")
        }
    }

    private func startListener(mapping: PortMapping) {
        // Defence in depth: if another mapping already owns this host port,
        // refuse to start a second listener so traffic is not silently mis-routed.
        if let existing = existingListener(for: mapping) {
            print("[port-forwarder] ERROR: host port \(mapping.hostPort) already forwarded by \(existing.namespace)/\(existing.name ?? "?"); refusing \(mapping.namespace)/\(mapping.name ?? "?")")
            return
        }

        let listener = Listener(mapping: mapping)
        listenersLock.lock()
        listeners[mapping.listenerKey] = listener
        listenersLock.unlock()
        listener.start { [weak self] in
            self?.stopListener(key: mapping.listenerKey)
        }
        print("[port-forwarder] forwarding localhost:\(mapping.hostPort) -> \(mapping.guestIP):\(mapping.hostPort)")
    }

    private func existingListener(for mapping: PortMapping) -> PortMapping? {
        let proto = mapping.protocol ?? "tcp"
        listenersLock.lock()
        defer { listenersLock.unlock() }
        for (_, listener) in listeners {
            let other = listener.mapping
            if other.hostPort == mapping.hostPort && (other.protocol ?? "tcp") == proto {
                return other
            }
        }
        return nil
    }

    private func stopListener(key: String) {
        listenersLock.lock()
        guard let listener = listeners.removeValue(forKey: key) else {
            listenersLock.unlock()
            return
        }
        listenersLock.unlock()
        listener.stop()
    }
}

// MARK: - Per-port listener

private final class Listener {
    let mapping: PortMapping
    private var fd: Int32 = -1
    private let lock = NSLock()

    init(mapping: PortMapping) {
        self.mapping = mapping
    }

    deinit {
        stop()
    }

    func start(onFailure: @escaping () -> Void) {
        DispatchQueue.global(qos: .utility).async { [weak self] in
            guard let self = self else { return }
            let fd = socket(AF_INET6, SOCK_STREAM, 0)
            guard fd >= 0 else {
                print("[listener :\(self.mapping.hostPort)] socket failed")
                onFailure()
                return
            }

            var noSigPipe: Int32 = 1
            setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
            var reuse: Int32 = 1
            setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &reuse, socklen_t(MemoryLayout<Int32>.size))
            var nodelay: Int32 = 1
            setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &nodelay, socklen_t(MemoryLayout<Int32>.size))

            // Allow binding to both IPv4 and IPv6 localhost.
            var off: Int32 = 0
            setsockopt(fd, IPPROTO_IPV6, IPV6_V6ONLY, &off, socklen_t(MemoryLayout<Int32>.size))

            var addr = sockaddr_in6()
            addr.sin6_family = sa_family_t(AF_INET6)
            addr.sin6_port = in_port_t(self.mapping.hostPort).bigEndian
            addr.sin6_addr = in6addr_any

            let bindResult = withUnsafePointer(to: &addr) { ptr -> Int32 in
                ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                    bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in6>.size))
                }
            }
            guard bindResult == 0, listen(fd, 128) == 0 else {
                print("[listener :\(self.mapping.hostPort)] bind/listen failed: \(String(cString: strerror(errno)))")
                close(fd)
                onFailure()
                return
            }

            self.lock.lock()
            self.fd = fd
            self.lock.unlock()

            while self.isRunning {
                var clientAddr = sockaddr_in6()
                var len = socklen_t(MemoryLayout<sockaddr_in6>.size)
                let client = withUnsafeMutablePointer(to: &clientAddr) { ptr -> Int32 in
                    ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                        accept(fd, $0, &len)
                    }
                }
                guard client >= 0 else {
                    if errno == EINTR { continue }
                    break
                }
                self.handleClient(client)
            }

            close(fd)
        }
    }

    func stop() {
        lock.lock()
        if fd >= 0 {
            close(fd)
            fd = -1
        }
        lock.unlock()
        // Closing the listener fd is enough to unblock accept; relay sockets will be
        // closed when the next read/write fails.
    }

    private var isRunning: Bool {
        lock.lock()
        let running = fd >= 0
        lock.unlock()
        return running
    }

    private func handleClient(_ clientFd: Int32) {
        var noSigPipe: Int32 = 1
        setsockopt(clientFd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
        var nodelay: Int32 = 1
        setsockopt(clientFd, IPPROTO_TCP, TCP_NODELAY, &nodelay, socklen_t(MemoryLayout<Int32>.size))

        let targetFd = connectToGuest()
        guard targetFd >= 0 else {
            close(clientFd)
            return
        }
        setsockopt(targetFd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
        setsockopt(targetFd, IPPROTO_TCP, TCP_NODELAY, &nodelay, socklen_t(MemoryLayout<Int32>.size))

        let group = DispatchGroup()
        relay(group: group, from: clientFd, to: targetFd)
        relay(group: group, from: targetFd, to: clientFd)

        group.notify(queue: .global(qos: .utility)) {
            close(clientFd)
            close(targetFd)
        }
    }

    private func connectToGuest() -> Int32 {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { return -1 }

        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = in_port_t(mapping.hostPort).bigEndian
        guard mapping.guestIP.withCString({ inet_pton(AF_INET, $0, &addr.sin_addr) }) == 1 else {
            close(fd)
            return -1
        }

        let connected = withUnsafePointer(to: &addr) { ptr -> Bool in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) == 0
            }
        }
        guard connected else {
            close(fd)
            return -1
        }
        return fd
    }

    private func relay(group: DispatchGroup, from: Int32, to: Int32) {
        group.enter()
        DispatchQueue.global(qos: .utility).async {
            var buffer = [UInt8](repeating: 0, count: 65536)
            while true {
                let n = recv(from, &buffer, buffer.count, 0)
                if n <= 0 { break }
                var sent = 0
                let written = buffer.withUnsafeBufferPointer { ptr -> Int in
                    while sent < n {
                        let w = send(to, ptr.baseAddress!.advanced(by: sent), n - sent, 0)
                        if w <= 0 { break }
                        sent += w
                    }
                    return sent
                }
                if written < n { break }
            }
            shutdown(to, SHUT_WR)
            group.leave()
        }
    }
}

// MARK: - FD write helper

private func writeAllFD(_ fd: Int32, data: Data) -> Bool {
    var total = 0
    let ok = data.withUnsafeBytes { raw -> Bool in
        guard let base = raw.baseAddress else { return false }
        while total < data.count {
            let n = write(fd, base.advanced(by: total), data.count - total)
            if n < 0 { return false }
            if n == 0 { return false }
            total += n
        }
        return true
    }
    return ok
}
