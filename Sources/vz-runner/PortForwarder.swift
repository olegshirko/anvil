import Foundation
import Virtualization
import Darwin

/// The guest-agent TCP port the port proxy listens on (see portproxy.go in
/// guest-agent). The forwarder connects here and sends a length-prefixed
/// JSON header describing the real target (containerIP:containerPort).
let guestPortProxyPort: Int = 39131

struct PortMapping: Codable, Hashable {
    let namespace: String
    let containerID: String
    let name: String?
    let hostPort: Int
    let containerPort: Int
    let `protocol`: String?
    let guestIP: String
    let containerIP: String?
    /// Host address from `-p <hostIP>:<host>:<container>`; nil, empty,
    /// 0.0.0.0 or :: mean every interface.
    var hostIP: String? = nil

    enum CodingKeys: String, CodingKey {
        case namespace
        case containerID = "container_id"
        case name
        case hostPort = "host_port"
        case containerPort = "container_port"
        case `protocol`
        case guestIP = "guest_ip"
        case containerIP = "container_ip"
        case hostIP = "host_ip"
    }

    /// Listener identity key. A single host-side listener is bound per exposed host port.
    var listenerKey: String {
        let proto = `protocol` ?? "tcp"
        return "\(namespace)/\(containerID)/\(hostPort)/\(proto)"
    }
}

/// Where a host listener binds. Listeners use one AF_INET6 socket type for
/// every address: an IPv4 host IP binds its IPv4-mapped form on a dual-stack
/// socket, which accepts IPv4 traffic to that address only — so
/// `-p 127.0.0.1:5432:5432` stays on loopback instead of the whole LAN.
struct ListenerBindAddress {
    let address: in6_addr
    let v6Only: Bool

    init?(hostIP: String?) {
        let ip = (hostIP ?? "").trimmingCharacters(in: CharacterSet(charactersIn: "[] "))
        if ip.isEmpty || ip == "0.0.0.0" || ip == "::" {
            address = in6addr_any
            v6Only = false
            return
        }
        var v4 = in_addr()
        if inet_pton(AF_INET, ip, &v4) == 1 {
            var mapped = in6_addr()
            withUnsafeMutableBytes(of: &mapped) { raw in
                raw[10] = 0xff
                raw[11] = 0xff
                withUnsafeBytes(of: v4) { raw[12..<16].copyBytes(from: $0) }
            }
            address = mapped
            v6Only = false
            return
        }
        var v6 = in6_addr()
        if inet_pton(AF_INET6, ip, &v6) == 1 {
            address = v6
            v6Only = true
            return
        }
        return nil
    }

    /// Apply IPV6_V6ONLY and bind `fd` to this address and `port`.
    func bind(fd: Int32, port: Int) -> Int32 {
        var only: Int32 = v6Only ? 1 : 0
        setsockopt(fd, IPPROTO_IPV6, IPV6_V6ONLY, &only, socklen_t(MemoryLayout<Int32>.size))
        var addr = sockaddr_in6()
        addr.sin6_family = sa_family_t(AF_INET6)
        addr.sin6_port = in_port_t(port).bigEndian
        addr.sin6_addr = address
        return withUnsafePointer(to: &addr) { ptr -> Int32 in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in6>.size))
            }
        }
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
        // Diff on the full mapping, not just the listener key: a container
        // restart keeps its ID (same key) but usually gets a new CNI address,
        // and the listener dials the IP captured at creation.
        var desiredByKey: [String: PortMapping] = [:]
        for mapping in state.mappings where desiredByKey[mapping.listenerKey] == nil {
            desiredByKey[mapping.listenerKey] = mapping
        }

        listenersLock.lock()
        let current: [String: PortMapping] = listeners.mapValues { $0.mapping }
        listenersLock.unlock()

        let desiredKeys = Set(desiredByKey.keys)
        let currentKeys = Set(current.keys)
        let toRemove = currentKeys.subtracting(desiredKeys)
        // Same key but changed target (new container IP after a restart) —
        // the listener must be rebuilt, not left dialing the old address.
        let toRestart = currentKeys.intersection(desiredKeys).filter { current[$0] != desiredByKey[$0] }
        let toAdd = desiredKeys.subtracting(currentKeys).union(toRestart)

        // Removal runs before (re)addition so a same-port replacement never
        // hits the "port already forwarded" guard in startListener.
        for key in toRemove.union(toRestart) {
            stopListener(key: key)
        }
        for key in toAdd {
            if let mapping = desiredByKey[key] {
                startListener(mapping: mapping)
            }
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
        if let target = mapping.containerIP, !target.isEmpty {
            print("[port-forwarder] forwarding \(mapping.hostIP ?? "*"):\(mapping.hostPort) -> proxy -> \(target):\(mapping.containerPort)")
        } else {
            print("[port-forwarder] forwarding localhost:\(mapping.hostPort) -> \(mapping.guestIP):\(mapping.hostPort)")
        }
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

    /// Whether a host TCP port is currently bound by one of our own listeners.
    func holdsTCP(port: Int) -> Bool {
        listenersLock.lock()
        defer { listenersLock.unlock() }
        return listeners.values.contains {
            $0.mapping.hostPort == port && ($0.mapping.protocol ?? "tcp") == "tcp"
        }
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
            if (self.mapping.protocol ?? "tcp") == "udp" {
                self.startUDP(onFailure: onFailure)
                return
            }
            guard let bindAddress = ListenerBindAddress(hostIP: self.mapping.hostIP) else {
                print("[listener :\(self.mapping.hostPort)] invalid host IP \(self.mapping.hostIP ?? ""); refusing to start")
                onFailure()
                return
            }
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

            let bindResult = bindAddress.bind(fd: fd, port: self.mapping.hostPort)
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
        udpClientsLock.lock()
        for (_, c) in udpClients {
            close(c.fd)
        }
        udpClients.removeAll()
        udpClientsLock.unlock()
        // Closing the listener fd is enough to unblock accept; relay sockets will be
        // closed when the next read/write fails.
    }

    // MARK: - UDP relay

    // One datagram socket per host port; for every distinct client endpoint
    // a connected socket toward the guest target is created, and datagrams
    // are pumped in both directions. VZ NAT passes host->guest UDP, and the
    // target is the container's CNI address (containerIP:containerPort) —
    // reachable from the host for UDP via the NAT gateway, unlike TCP, which
    // needs the in-guest port proxy. Client sockets idle out after 60 s.
    private struct UDPClient {
        let fd: Int32
        var lastUsed: Date
    }

    private var udpClients: [String: UDPClient] = [:]
    private let udpClientsLock = NSLock()
    private let udpIdleTimeout: TimeInterval = 60

    /// Stable dictionary key for a client address: the raw socket bytes.
    private func udpKey(_ addr: sockaddr_in6) -> String {
        withUnsafeBytes(of: addr) { raw in
            raw.map { String($0) }.joined(separator: ",")
        }
    }

    private func startUDP(onFailure: @escaping () -> Void) {
        // Published UDP ports are served by the guest itself: CNI portmap
        // arms nft DNAT hostPort -> containerPort in the container's netns
        // at attach time. The host side therefore only needs a datagram
        // listener forwarding to guestIP:hostPort, which vzNAT delivers.
        guard !mapping.guestIP.isEmpty else {
            print("[listener :\(mapping.hostPort)/udp] no guest IP; refusing to start")
            onFailure()
            return
        }
        guard let bindAddress = ListenerBindAddress(hostIP: mapping.hostIP) else {
            print("[listener :\(mapping.hostPort)/udp] invalid host IP \(mapping.hostIP ?? ""); refusing to start")
            onFailure()
            return
        }
        let fd = socket(AF_INET6, SOCK_DGRAM, 0)
        guard fd >= 0 else {
            onFailure()
            return
        }
        var reuse: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &reuse, socklen_t(MemoryLayout<Int32>.size))
        // Wake the recvfrom loop regularly so replies can be pumped even
        // when no new client datagrams arrive.
        var tv = timeval(tv_sec: 0, tv_usec: 250_000)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))

        let bindResult = bindAddress.bind(fd: fd, port: mapping.hostPort)
        guard bindResult == 0 else {
            print("[listener :\(mapping.hostPort)/udp] bind failed: \(String(cString: strerror(errno)))")
            close(fd)
            onFailure()
            return
        }
        lock.lock()
        self.fd = fd
        lock.unlock()
        let targetIP = mapping.guestIP
        let targetPort = mapping.hostPort
        print("[port-forwarder] forwarding localhost:\(mapping.hostPort)/udp -> \(targetIP):\(targetPort) (guest relay)")
        udpRelayLoop(fd, targetIP: targetIP, targetPort: targetPort)
    }

    private func udpRelayLoop(_ fd: Int32, targetIP: String, targetPort: Int) {
        var buffer = [UInt8](repeating: 0, count: 65536)
        while isRunning {
            var src = sockaddr_in6()
            var srcLen = socklen_t(MemoryLayout<sockaddr_in6>.size)
            let n = withUnsafeMutablePointer(to: &src) { ptr -> Int in
                ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                    recvfrom(fd, &buffer, buffer.count, 0, $0, &srcLen)
                }
            }
            if n > 0 {
                let clientFd = udpClientFd(for: src, targetIP: targetIP, targetPort: targetPort)
                if clientFd >= 0 {
                    _ = buffer.withUnsafeBufferPointer { ptr in
                        send(clientFd, ptr.baseAddress, n, 0)
                    }
                }
            } else if n < 0 && errno != EAGAIN && errno != EWOULDBLOCK && errno != EINTR {
                break
            }
            pumpUDPReplies(fd: fd)
            reapIdleUDPClients()
        }
    }

    /// Drains pending inbound datagrams from every per-client guest socket
    /// and sends them back to the remembered client via the shared socket.
    private func pumpUDPReplies(fd: Int32) {
        udpClientsLock.lock()
        let clients = udpClients
        let keysByFd = Dictionary(uniqueKeysWithValues: udpClients.map { ($0.value.fd, $0.key) })
        udpClientsLock.unlock()
        var buffer = [UInt8](repeating: 0, count: 65536)
        for (key, c) in clients {
            while true {
                let n = recv(c.fd, &buffer, buffer.count, MSG_DONTWAIT)
                if n <= 0 { break }
                if let client = udpClientAddr(for: key) {
                    _ = withUnsafePointer(to: client) { ptr in
                        ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                            sendto(fd, &buffer, n, 0, $0, socklen_t(MemoryLayout<sockaddr_in6>.size))
                        }
                    }
                }
                markUDPActive(key)
            }
        }
    }

    /// Reconstructs a sockaddr_in6 from its dictionary key (inverse of udpKey).
    private func udpClientAddr(for key: String) -> sockaddr_in6? {
        var addr = sockaddr_in6()
        let parts = key.split(separator: ",").compactMap { UInt8($0) }
        guard parts.count == MemoryLayout<sockaddr_in6>.size else { return nil }
        withUnsafeMutableBytes(of: &addr) { raw in
            parts.withUnsafeBufferPointer { src in
                raw.copyBytes(from: src)
            }
        }
        return addr
    }

    /// Returns (creating if needed) a connected socket to the guest target
    /// for the given client endpoint.
    private func udpClientFd(for client: sockaddr_in6, targetIP: String, targetPort: Int) -> Int32 {
        let key = udpKey(client)
        udpClientsLock.lock()
        if let c = udpClients[key], c.fd >= 0 {
            udpClients[key]?.lastUsed = Date()
            udpClientsLock.unlock()
            return c.fd
        }
        udpClientsLock.unlock()

        let fd = socket(AF_INET, SOCK_DGRAM, 0)
        guard fd >= 0 else { return -1 }
        var target = sockaddr_in()
        target.sin_family = sa_family_t(AF_INET)
        target.sin_port = in_port_t(targetPort).bigEndian
        guard targetIP.withCString({ inet_pton(AF_INET, $0, &target.sin_addr) }) == 1 else {
            close(fd)
            return -1
        }
        let connected = withUnsafePointer(to: &target) { ptr -> Bool in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) == 0
            }
        }
        guard connected else {
            close(fd)
            return -1
        }
        udpClientsLock.lock()
        udpClients[key] = UDPClient(fd: fd, lastUsed: Date())
        udpClientsLock.unlock()
        return fd
    }

    private func markUDPActive(_ key: String) {
        udpClientsLock.lock()
        udpClients[key]?.lastUsed = Date()
        udpClientsLock.unlock()
    }

    private func reapIdleUDPClients() {
        udpClientsLock.lock()
        let now = Date()
        for (k, c) in udpClients where now.timeIntervalSince(c.lastUsed) > udpIdleTimeout {
            close(c.fd)
            udpClients.removeValue(forKey: k)
        }
        udpClientsLock.unlock()
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

        // Preferred path: the guest-side port proxy. The forwarder connects
        // to a single well-known port and names the real target
        // (containerIP:containerPort) in a length-prefixed JSON header —
        // user host ports are never bound inside the guest, so published
        // ports cannot conflict with live containers.
        // Fallback (containers created before the proxy existed / snapshots
        // from older guests): dial guestIP:hostPort directly, where CNI DNAT
        // still answers.
        let useProxy = containerProxyTarget()

        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        if let proxy = useProxy {
            addr.sin_port = in_port_t(guestPortProxyPort).bigEndian
            guard mapping.guestIP.withCString({ inet_pton(AF_INET, $0, &addr.sin_addr) }) == 1 else {
                close(fd)
                return -1
            }
            guard connectWithTimeout(fd, addr, timeout: 5.0) else {
                close(fd)
                return -1
            }
            guard sendPortProxyHeader(fd, target: proxy) else {
                close(fd)
                return -1
            }
            return fd
        }

        addr.sin_port = in_port_t(mapping.hostPort).bigEndian
        guard mapping.guestIP.withCString({ inet_pton(AF_INET, $0, &addr.sin_addr) }) == 1 else {
            close(fd)
            return -1
        }

        guard connectWithTimeout(fd, addr, timeout: 5.0) else {
            close(fd)
            return -1
        }
        return fd
    }

    /// The proxy target when the mapping carries a container IP (the CNI
    /// address, reachable only from inside the guest).
    private func containerProxyTarget() -> (ip: String, port: Int)? {
        guard let ip = mapping.containerIP, !ip.isEmpty, mapping.containerPort > 0 else {
            return nil
        }
        return (ip, mapping.containerPort)
    }

    /// Sends the port-proxy handshake: 4-byte big-endian body length + JSON.
    private func sendPortProxyHeader(_ fd: Int32, target: (ip: String, port: Int)) -> Bool {
        let header = "{\"container_ip\":\"\(target.ip)\",\"container_port\":\(target.port)}"
        let body = Array(header.utf8)

        var length = UInt32(body.count).bigEndian
        let lengthSent = withUnsafeBytes(of: &length) { ptr -> Int in
            var total = 0
            while total < 4 {
                let n = write(fd, ptr.baseAddress!.advanced(by: total), 4 - total)
                if n <= 0 { return -1 }
                total += n
            }
            return total
        }
        guard lengthSent == 4 else { return false }

        var mutableBody = body
        let bodySent = mutableBody.withUnsafeMutableBufferPointer { ptr -> Int in
            var total = 0
            while total < body.count {
                let n = write(fd, ptr.baseAddress!.advanced(by: total), body.count - total)
                if n <= 0 { return -1 }
                total += n
            }
            return total
        }
        return bodySent == body.count
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

/// connect(2) with a timeout, via non-blocking mode + poll. A blocking
/// connect to an unreachable guest address stalls for the full TCP SYN
/// timeout (~75 s), and since it runs inline in the listener's accept loop
/// it takes the whole published port down with it.
private func connectWithTimeout(_ fd: Int32, _ addr: sockaddr_in, timeout: TimeInterval) -> Bool {
    let flags = fcntl(fd, F_GETFL, 0)
    guard flags >= 0 else { return false }
    fcntl(fd, F_SETFL, flags | O_NONBLOCK)

    let rc = withUnsafePointer(to: addr) { ptr -> Int32 in
        ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
        }
    }
    if rc == 0 {
        fcntl(fd, F_SETFL, flags)
        return true
    }
    guard errno == EINPROGRESS else { return false }

    var pfd = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
    guard poll(&pfd, 1, Int32(timeout * 1000)) == 1 else { return false }
    var soError: Int32 = 0
    var len = socklen_t(MemoryLayout<Int32>.size)
    getsockopt(fd, SOL_SOCKET, SO_ERROR, &soError, &len)
    // Restore blocking mode: the relay pumps rely on blocking recv/send.
    fcntl(fd, F_SETFL, flags)
    return soError == 0
}

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
