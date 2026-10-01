import Foundation
import Virtualization

// Connections the guest opens to host services (egress 1028, ssh-agent
// 1029, host loopback 1030). Anything in the VM — a container through
// host.docker.internal included — can open them, and each one can block
// for as long as its peer likes. They therefore never run on GCD's shared
// pool (a few dozen stuck relays there would starve the whole daemon):
// every connection gets its own thread, relaying both directions with
// poll(), and the number of live ones is capped.

/// Upper bound on concurrent guest-initiated relays per service: a
/// container holding host.docker.internal connections open must not lock
/// out egress (pulls behind a VPN), ssh-agent or the port check.
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

private let guestLimitersLock = NSLock()
nonisolated(unsafe) private var guestLimiters: [String: ConnectionLimiter] = [:]

/// The limiter of one guest service (by connection name).
func guestConnectionLimiter(_ service: String) -> ConnectionLimiter {
    guestLimitersLock.lock()
    defer { guestLimitersLock.unlock() }
    if let l = guestLimiters[service] { return l }
    let l = ConnectionLimiter(limit: maxGuestConnections)
    guestLimiters[service] = l
    return l
}

/// Cap on concurrent Docker API / buildkit socket clients. Long-lived
/// docker clients (logs -f, events, attach) are legitimate, hence the
/// higher bound.
let hostConnectionLimiter = ConnectionLimiter(limit: 1024)

/// Separate cap for published-port connections: those listen on every
/// interface by default, and a LAN peer holding them open must not lock the
/// Docker CLI out of docker.sock.
let portConnectionLimiter = ConnectionLimiter(limit: 1024)

/// Larger socket buffers for the local Docker/buildkit sockets: macOS
/// AF_UNIX defaults to 8 KiB, so build contexts, `docker load` and
/// `docker cp` moved in 8 KiB reads whatever the relay buffer size.
func setLargeSocketBuffers(_ fd: Int32) {
    var size: Int32 = 1 << 20
    _ = setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &size, socklen_t(MemoryLayout<Int32>.size))
    _ = setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &size, socklen_t(MemoryLayout<Int32>.size))
}

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
                        limiter: ConnectionLimiter? = nil,
                        _ body: @escaping () -> Void) {
    let limiter = limiter ?? guestConnectionLimiter(name)
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

/// Copy bytes in both directions until both sides are done, half-closing
/// the peer when one direction ends. Each direction gets its own thread
/// with blocking I/O: a single thread that blocks writing to one side stops
/// draining the other, and a stream whose output grows with its input
/// (`docker exec -i c xxd` fed a large file) deadlocked once both socket
/// buffers filled. The calling thread pumps a -> b.
func relayBothWays(_ a: Int32, _ b: Int32) {
    let reverseDone = DispatchSemaphore(value: 0)
    let reverse = Thread {
        pumpOneWay(from: b, to: a)
        reverseDone.signal()
    }
    reverse.name = "relay-reverse"
    reverse.stackSize = 256 * 1024
    reverse.start()
    pumpOneWay(from: a, to: b)
    reverseDone.wait()
}

/// Copy `from` to `to` until EOF, then half-close `to`. When `to` is gone
/// both sockets are shut down, which also ends the opposite pump.
func pumpOneWay(from: Int32, to: Int32) {
    var buf = [UInt8](repeating: 0, count: 65536)
    while true {
        let n = read(from, &buf, buf.count)
        if n < 0 && errno == EINTR { continue }
        if n <= 0 {
            _ = shutdown(to, Int32(SHUT_WR))
            return
        }
        var off = 0
        while off < n {
            let w = buf.withUnsafeBytes { write(to, $0.baseAddress!.advanced(by: off), n - off) }
            if w < 0 && errno == EINTR { continue }
            if w <= 0 {
                // The peer is gone: unblock the other direction too.
                _ = shutdown(to, Int32(SHUT_RDWR))
                _ = shutdown(from, Int32(SHUT_RDWR))
                return
            }
            off += w
        }
    }
}
