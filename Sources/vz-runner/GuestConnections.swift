import Foundation
import Virtualization

// Connections the guest opens to host services (egress 1028, ssh-agent
// 1029, host loopback 1030). Anything in the VM — a container through
// host.docker.internal included — can open them, and each one can block
// for as long as its peer likes. They therefore never run on GCD's shared
// pool (a few dozen stuck relays there would starve the whole daemon):
// every connection gets its own thread, relaying both directions with
// poll(), and the number of live ones is capped.

/// Upper bound on concurrent guest-initiated relays across all services.
let maxGuestConnections = 256

/// A handshake (the length-prefixed request) must arrive within this.
let guestHandshakeTimeout = 10

final class ConnectionLimiter {
    private let lock = NSLock()
    private var active = 0
    private var warned = false
    let limit: Int

    init(limit: Int) {
        self.limit = limit
    }

    func tryAcquire() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        guard active < limit else {
            if !warned {
                warned = true
                print("[host-services] \(limit) guest connections open; refusing more until some close")
            }
            return false
        }
        active += 1
        return true
    }

    func release() {
        lock.lock()
        active -= 1
        if active < limit / 2 { warned = false }
        lock.unlock()
    }

    var count: Int {
        lock.lock()
        defer { lock.unlock() }
        return active
    }
}

let guestConnectionLimiter = ConnectionLimiter(limit: maxGuestConnections)

/// Cap on concurrent host-side client connections (docker.sock, buildkit
/// sock, published ports). Published ports listen on all interfaces by
/// default, so LAN peers can open these; long-lived docker clients (logs -f,
/// events, attach) are legitimate, hence the higher bound.
let hostConnectionLimiter = ConnectionLimiter(limit: 1024)

/// Run body on a dedicated thread (never GCD's shared pool: blocking relays
/// there starve the whole daemon). Over the limiter's cap, onReject runs
/// instead, on the caller's thread.
func runOnConnectionThread(name: String, limiter: ConnectionLimiter,
                           onReject: () -> Void, _ body: @escaping () -> Void) {
    guard limiter.tryAcquire() else {
        onReject()
        return
    }
    let thread = Thread {
        defer { limiter.release() }
        body()
    }
    thread.name = name
    thread.stackSize = 256 * 1024
    thread.start()
}

/// Start a long-running loop (an accept loop) on its own thread.
func startLoopThread(name: String, _ body: @escaping () -> Void) {
    let thread = Thread(block: body)
    thread.name = name
    thread.stackSize = 256 * 1024
    thread.start()
}

/// Run body for a guest connection on a dedicated thread, closing the
/// connection afterwards. Over the cap the connection is closed at once.
func runGuestConnection(_ connection: VZVirtioSocketConnection, name: String,
                        limiter: ConnectionLimiter = guestConnectionLimiter,
                        _ body: @escaping () -> Void) {
    guard limiter.tryAcquire() else {
        connection.close()
        return
    }
    let thread = Thread {
        defer {
            connection.close()
            limiter.release()
        }
        body()
    }
    thread.name = "guest-\(name)"
    thread.stackSize = 256 * 1024
    thread.start()
}

/// Run body with SO_RCVTIMEO set on fd, then clear it again.
func withReceiveTimeout<T>(_ fd: Int32, seconds: Int, _ body: () throws -> T) throws -> T {
    var tv = timeval(tv_sec: seconds, tv_usec: 0)
    _ = setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
    defer {
        var zero = timeval(tv_sec: 0, tv_usec: 0)
        _ = setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &zero, socklen_t(MemoryLayout<timeval>.size))
    }
    return try body()
}

/// Copy bytes in both directions on the calling thread until both sides
/// are done, half-closing the peer when one direction ends.
func relayBothWays(_ a: Int32, _ b: Int32) {
    var buf = [UInt8](repeating: 0, count: 65536)
    var aOpen = true // a -> b still flowing
    var bOpen = true // b -> a still flowing
    while aOpen || bOpen {
        var fds: [pollfd] = []
        if aOpen { fds.append(pollfd(fd: a, events: Int16(POLLIN), revents: 0)) }
        if bOpen { fds.append(pollfd(fd: b, events: Int16(POLLIN), revents: 0)) }
        let ready = poll(&fds, nfds_t(fds.count), -1)
        if ready < 0 {
            if errno == EINTR { continue }
            return
        }
        for p in fds where p.revents != 0 {
            let from = p.fd
            let to = from == a ? b : a
            let n = read(from, &buf, buf.count)
            if n < 0 && (errno == EINTR || errno == EAGAIN) { continue }
            if n <= 0 {
                _ = shutdown(to, Int32(SHUT_WR))
                if from == a { aOpen = false } else { bOpen = false }
                continue
            }
            var off = 0
            while off < n {
                let w = buf.withUnsafeBytes { write(to, $0.baseAddress!.advanced(by: off), n - off) }
                if w < 0 && errno == EINTR { continue }
                if w <= 0 { return } // the peer is gone: stop both directions
                off += w
            }
        }
    }
}
