import Foundation

/// Set by the daemon: its output is a log, and lines without times made
/// correlating restarts, kills and guest events guesswork. CLI commands
/// keep plain output.
nonisolated(unsafe) var timestampedOutput = false

private let logTimeFormatter: DateFormatter = {
    let f = DateFormatter()
    f.locale = Locale(identifier: "en_US_POSIX")
    f.dateFormat = "yyyy-MM-dd'T'HH:mm:ss.SSSZZZZZ"
    return f
}()
private let logTimeLock = NSLock()

/// Shadows Swift.print for the whole module.
func print(_ items: Any..., separator: String = " ", terminator: String = "\n") {
    let text = items.map { "\($0)" }.joined(separator: separator)
    guard timestampedOutput else {
        Swift.print(text, terminator: terminator)
        return
    }
    logTimeLock.lock()
    let stamp = logTimeFormatter.string(from: Date())
    logTimeLock.unlock()
    Swift.print("\(stamp) \(text)", terminator: terminator)
}
