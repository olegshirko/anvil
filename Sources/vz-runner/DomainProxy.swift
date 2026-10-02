import Foundation

/// One running container in the guest's domain table.
struct DomainEntry: Codable, Equatable {
    let names: [String]
    let ip: String
    let port: Int
}

/// Serves http://<name>.anvil.localhost on the Mac (opt-in: ANVIL_DOMAINS=1,
/// port ANVIL_DOMAINS_PORT, default 80). Browsers and curl resolve any
/// *.localhost name to the loopback, so a container is one URL away without
/// publishing a port: <container>.anvil.localhost, or
/// <service>.<project>.anvil.localhost for compose. The request's Host picks
/// the container; the connection is then relayed unchanged (keep-alive,
/// WebSocket upgrades) through the guest's port proxy to the container's
/// HTTP port (guest-side choice: label dev.anvil.http.port, else EXPOSE).
final class DomainProxy {
    static let suffix = ".anvil.localhost"

    private let port: Int
    private let hooks: PortConnectionHooks
    private let lock = NSLock()
    private var table: [String: DomainEntry] = [:]
    private var guestIP = ""
    /// Set by the first push: an empty table then means no containers, not
    /// "not known yet".
    private var tableReceived = false
    private var fds: [Int32] = []

    init(port: Int, hooks: PortConnectionHooks) {
        self.port = port
        self.hooks = hooks
    }

    /// ANVIL_DOMAINS / ANVIL_DOMAINS_PORT; nil when the feature is off.
    static func configuredPort() -> Int? {
        guard let on = anvilSetting("ANVIL_DOMAINS")?.lowercased(), ["1", "true", "yes", "on"].contains(on) else {
            return nil
        }
        if let p = anvilSetting("ANVIL_DOMAINS_PORT").flatMap({ Int($0.trimmingCharacters(in: .whitespaces)) }),
           p > 0, p < 65536 {
            return p
        }
        return 80
    }

    func update(domains: [DomainEntry], guestIP: String) {
        var t: [String: DomainEntry] = [:]
        for d in domains {
            for n in d.names { t[n] = d }
        }
        lock.lock()
        table = t
        tableReceived = true
        if !guestIP.isEmpty { self.guestIP = guestIP }
        lock.unlock()
    }

    func start() {
        // Loopback only: the names resolve to it, and nothing on the LAN
        // should reach containers through this.
        for family in [AF_INET, AF_INET6] {
            let fd = socket(family, SOCK_STREAM, 0)
            guard fd >= 0 else { continue }
            var one: Int32 = 1
            setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, socklen_t(MemoryLayout<Int32>.size))
            var rc: Int32
            if family == AF_INET {
                var a = sockaddr_in()
                a.sin_family = sa_family_t(AF_INET)
                a.sin_port = in_port_t(port).bigEndian
                a.sin_addr = in_addr(s_addr: INADDR_LOOPBACK.bigEndian)
                rc = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                    bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
            } else {
                setsockopt(fd, IPPROTO_IPV6, IPV6_V6ONLY, &one, socklen_t(MemoryLayout<Int32>.size))
                var a = sockaddr_in6()
                a.sin6_family = sa_family_t(AF_INET6)
                a.sin6_port = in_port_t(port).bigEndian
                a.sin6_addr = in6addr_loopback
                rc = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                    bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in6>.size)) } }
            }
            guard rc == 0, listen(fd, 128) == 0 else {
                print("[domains] cannot listen on \(family == AF_INET ? "127.0.0.1" : "[::1]"):\(port): \(String(cString: strerror(errno)))")
                close(fd)
                continue
            }
            lock.lock()
            fds.append(fd)
            lock.unlock()
            startLoopThread(name: "domains-\(family)") { [weak self] in self?.acceptLoop(fd) }
        }
        print("[domains] serving http://<container>\(Self.suffix)\(port == 80 ? "" : ":\(port)")")
    }

    private func acceptLoop(_ fd: Int32) {
        while true {
            let client = accept(fd, nil, nil)
            if client < 0 {
                if errno == EINTR || errno == ECONNABORTED { continue }
                if errno == EMFILE || errno == ENFILE {
                    Thread.sleep(forTimeInterval: 0.1)
                    continue
                }
                return
            }
            var noSigPipe: Int32 = 1
            setsockopt(client, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
            runOnConnectionThread(name: "domain-conn", limiter: portConnectionLimiter, onReject: { close(client) }) { [weak self] in
                self?.serve(client)
            }
        }
    }

    private func serve(_ client: Int32) {
        defer { close(client) }
        // The request head, read up to its end (bounded), to learn the Host.
        var tv = timeval(tv_sec: 10, tv_usec: 0)
        setsockopt(client, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        var head = Data()
        var buf = [UInt8](repeating: 0, count: 8192)
        while head.count < 64 * 1024, head.range(of: Data("\r\n\r\n".utf8)) == nil {
            let n = read(client, &buf, buf.count)
            if n <= 0 { return }
            head.append(buf, count: n)
        }
        var zero = timeval(tv_sec: 0, tv_usec: 0)
        setsockopt(client, SOL_SOCKET, SO_RCVTIMEO, &zero, socklen_t(MemoryLayout<timeval>.size))

        guard let host = Self.hostHeader(head) else {
            respond(client, status: "400 Bad Request", body: "anvil: request without a Host header\n")
            return
        }
        hooks.connect()
        defer { hooks.disconnect() }
        hooks.resume() // a paused or released VM comes back first

        guard let name = Self.containerName(host) else {
            respond(client, status: "404 Not Found", body: "anvil: \(host) is not a <name>\(Self.suffix) address\n")
            return
        }
        // Right after a wake the guest may not have pushed its table yet.
        var entry: DomainEntry?
        var vmIP = ""
        for _ in 0..<30 {
            lock.lock()
            entry = table[name]
            vmIP = guestIP
            let known = tableReceived && !vmIP.isEmpty
            lock.unlock()
            if entry != nil && !vmIP.isEmpty { break }
            if known && entry == nil { break } // the table is there: a real miss
            Thread.sleep(forTimeInterval: 0.1)
        }
        guard let target = entry, !vmIP.isEmpty else {
            lock.lock()
            let portSuffix = port == 80 ? "" : ":\(port)"
            let known = table.keys.sorted().map { "  http://\($0)\(Self.suffix)\(portSuffix)" }.joined(separator: "\n")
            lock.unlock()
            respond(client, status: "404 Not Found",
                    body: "anvil: no running container named \(name)\n\(known.isEmpty ? "" : "running:\n\(known)\n")")
            return
        }

        let upstream = socket(AF_INET, SOCK_STREAM, 0)
        guard upstream >= 0 else { return }
        defer { close(upstream) }
        bindToGuestInterface(upstream, guestIP: vmIP)
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = in_port_t(guestPortProxyPort).bigEndian
        guard vmIP.withCString({ inet_pton(AF_INET, $0, &addr.sin_addr) }) == 1,
              connectWithTimeout(upstream, addr, timeout: 5),
              sendGuestPortProxyHeader(upstream, ip: target.ip, port: target.port),
              (try? writeExactlyFD(upstream, data: head)) != nil else {
            respond(client, status: "502 Bad Gateway", body: "anvil: cannot reach \(name) on port \(target.port)\n")
            return
        }
        relayBothWays(client, upstream)
    }

    /// The Host header's name, lowercased, without a port.
    static func hostHeader(_ head: Data) -> String? {
        guard let text = String(data: head, encoding: .utf8) ?? String(data: head, encoding: .isoLatin1) else {
            return nil
        }
        for line in text.components(separatedBy: "\r\n").dropFirst() {
            if line.isEmpty { break }
            let parts = line.split(separator: ":", maxSplits: 1)
            guard parts.count == 2, parts[0].lowercased() == "host" else { continue }
            var host = parts[1].trimmingCharacters(in: .whitespaces).lowercased()
            if let colon = host.lastIndex(of: ":"), !host.hasSuffix("]") {
                host = String(host[..<colon])
            }
            return host.hasSuffix(".") ? String(host.dropLast()) : host
        }
        return nil
    }

    /// "web.shop.anvil.localhost" -> "web.shop".
    static func containerName(_ host: String) -> String? {
        guard host.hasSuffix(suffix) else { return nil }
        let name = String(host.dropLast(suffix.count))
        return name.isEmpty ? nil : name
    }

    private func respond(_ fd: Int32, status: String, body: String) {
        let msg = "HTTP/1.1 \(status)\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: \(body.utf8.count)\r\nConnection: close\r\n\r\n\(body)"
        try? writeExactlyFD(fd, data: Data(msg.utf8))
    }
}
