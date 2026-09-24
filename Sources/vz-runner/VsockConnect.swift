import Foundation
import Virtualization

/// Holds the result of one `device.connect` attempt. The completion can
/// arrive after the waiter gave up; the box then closes the late connection
/// instead of leaking it (and instead of the completion writing a variable
/// the waiter's thread is reading).
final class VsockConnectBox {
    private let lock = NSLock()
    private var connection: VZVirtioSocketConnection?
    private var abandoned = false

    /// Called by the connect completion.
    func deliver(_ conn: VZVirtioSocketConnection) {
        lock.lock()
        let late = abandoned
        if !late {
            connection = conn
        }
        lock.unlock()
        if late {
            conn.close()
        }
    }

    /// Called by the waiter once: returns the connection, or nil and marks
    /// the attempt abandoned so a later delivery is closed.
    func take() -> VZVirtioSocketConnection? {
        lock.lock()
        defer { lock.unlock() }
        abandoned = true
        let conn = connection
        connection = nil
        return conn
    }
}

/// One bounded connect attempt. `device.connect` must run on the main queue.
func connectVsockOnce(device: VZVirtioSocketDevice, port: UInt32, timeout: TimeInterval) -> VZVirtioSocketConnection? {
    let box = VsockConnectBox()
    let sem = DispatchSemaphore(value: 0)
    DispatchQueue.main.async {
        device.connect(toPort: port) { result in
            if case .success(let conn) = result {
                box.deliver(conn)
            }
            sem.signal()
        }
    }
    _ = sem.wait(timeout: .now() + timeout)
    return box.take()
}

/// Retry connect attempts until `deadline` (the guest may still be booting
/// or resuming).
func connectVsock(device: VZVirtioSocketDevice, port: UInt32, until deadline: Date,
                  attemptTimeout: TimeInterval = 2) -> VZVirtioSocketConnection? {
    while Date() < deadline {
        if let conn = connectVsockOnce(device: device, port: port, timeout: attemptTimeout) {
            return conn
        }
        Thread.sleep(forTimeInterval: 0.2)
    }
    return nil
}
