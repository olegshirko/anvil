import CoreServices
import Foundation
import Virtualization

/// Forwards file changes made on the Mac into the VM, so inotify watchers in
/// containers (hot reload) see edits to bind-mounted directories. virtiofs
/// shows the guest the new content but the guest kernel never sees the
/// change happen.
///
/// The guest pushes the directories running containers bind-mount (the port
/// state's `watch_paths`); they are watched here with one FSEvents stream.
/// Changed paths go to the guest over a control connection (`fs_events`)
/// with their current times, and the guest re-applies those times, which
/// raises IN_ATTRIB there. The touch comes back as a metadata-only event,
/// which is ignored.
final class FSEventsForwarder {
    private let deviceProvider: () -> VZVirtioSocketDevice?
    private let queue = DispatchQueue(label: "com.olegshirko.anvil.fsevents", qos: .utility)
    private var stream: FSEventStreamRef?
    private var paths: [String] = []
    /// Directories forwarded whole, and single bind-mounted files (their
    /// directory is watched, only the file forwarded: a bind of
    /// ~/.gitconfig must not forward every change in the home directory).
    private var dirRoots: [String] = []
    private var fileRoots: Set<String> = []
    /// Changed path -> accumulated FSEvents flags, flushed in batches.
    private var pending: [String: FSEventStreamEventFlags] = [:]
    private var flushScheduled = false
    private var connection: VZVirtioSocketConnection?
    /// What was last forwarded per path. FSEvents flags are cumulative for
    /// a recently changed file, so the echo of the guest's touch still says
    /// "modified"; an unchanged mtime is what tells it apart.
    private var lastSent: [String: (mtime: Int64, at: Date)] = [:]

    init(deviceProvider: @escaping () -> VZVirtioSocketDevice?) {
        self.deviceProvider = deviceProvider
    }

    /// Watch exactly these directories (an empty list stops watching).
    func update(paths newPaths: [String]) {
        queue.async { [self] in
            let sorted = newPaths.sorted()
            guard sorted != paths else { return }
            paths = sorted
            restartStream()
            if paths.isEmpty {
                closeConnection()
            }
        }
    }

    func stop() {
        update(paths: [])
    }

    // MARK: - FSEvents

    private func restartStream() {
        if let s = stream {
            FSEventStreamStop(s)
            FSEventStreamInvalidate(s)
            FSEventStreamRelease(s)
            stream = nil
        }
        pending.removeAll()
        dirRoots = []
        fileRoots = []
        var watched = Set<String>()
        for p in paths {
            var isDir: ObjCBool = false
            guard FileManager.default.fileExists(atPath: p, isDirectory: &isDir) else { continue }
            if isDir.boolValue {
                dirRoots.append(p)
                watched.insert(p)
            } else {
                fileRoots.insert(p)
                watched.insert((p as NSString).deletingLastPathComponent)
            }
        }
        guard !watched.isEmpty else { return }
        var ctx = FSEventStreamContext(version: 0, info: Unmanaged.passUnretained(self).toOpaque(),
                                       retain: nil, release: nil, copyDescription: nil)
        let callback: FSEventStreamCallback = { _, info, count, eventPaths, eventFlags, _ in
            guard let info = info else { return }
            let me = Unmanaged<FSEventsForwarder>.fromOpaque(info).takeUnretainedValue()
            let paths = Unmanaged<CFArray>.fromOpaque(eventPaths).takeUnretainedValue() as? [String] ?? []
            for i in 0..<min(count, paths.count) {
                me.record(path: paths[i], flags: eventFlags[i])
            }
        }
        let flags = UInt32(kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer
            | kFSEventStreamCreateFlagUseCFTypes)
        guard let s = FSEventStreamCreate(kCFAllocatorDefault, callback, &ctx, Array(watched) as CFArray,
                                          FSEventStreamEventId(kFSEventStreamEventIdSinceNow), 0.05, flags) else {
            print("[fsevents] cannot watch \(paths.count) directories")
            return
        }
        FSEventStreamSetDispatchQueue(s, queue)
        if FSEventStreamStart(s) {
            stream = s
            print("[fsevents] watching \(paths.count) bind-mounted directories")
        } else {
            FSEventStreamInvalidate(s)
            FSEventStreamRelease(s)
        }
    }

    /// Runs on `queue` (the stream's dispatch queue).
    private func record(path: String, flags: FSEventStreamEventFlags) {
        let content = FSEventStreamEventFlags(kFSEventStreamEventFlagItemCreated | kFSEventStreamEventFlagItemRemoved
            | kFSEventStreamEventFlagItemRenamed | kFSEventStreamEventFlagItemModified)
        // Metadata-only events are the echo of the guest's own touch (and
        // chmods/xattrs, which watchers do not need).
        guard flags & content != 0 else { return }
        guard fileRoots.contains(path) || dirRoots.contains(where: { path == $0 || path.hasPrefix($0 + "/") }) else {
            return
        }
        pending[path, default: 0] |= flags
        if !flushScheduled {
            flushScheduled = true
            queue.asyncAfter(deadline: .now() + 0.05) { [self] in flush() }
        }
    }

    private struct Event: Encodable {
        let p: String
        let a: Int64
        let m: Int64
    }

    private struct Batch: Encodable {
        let events: [Event]
    }

    private func flush() {
        flushScheduled = false
        let changes = pending
        pending.removeAll()
        var events: [Event] = []
        var seen = Set<String>()
        let structural = FSEventStreamEventFlags(kFSEventStreamEventFlagItemCreated
            | kFSEventStreamEventFlagItemRemoved | kFSEventStreamEventFlagItemRenamed)
        let now = Date()
        lastSent = lastSent.filter { now.timeIntervalSince($0.value.at) < 30 }
        func add(_ p: String) {
            guard seen.insert(p).inserted, let ev = Self.timesEvent(p) else { return }
            if let last = lastSent[p], last.mtime == ev.m {
                return // our own touch coming back, or no real change
            }
            lastSent[p] = (ev.m, now)
            events.append(ev)
        }
        for (path, flags) in changes {
            add(path)
            // A watcher of the directory learns of new, gone and renamed
            // entries through the directory itself.
            if flags & structural != 0 {
                add((path as NSString).deletingLastPathComponent)
            }
        }
        guard !events.isEmpty else { return }
        // Bounded frames: a checkout touching thousands of files goes in parts.
        for start in stride(from: 0, to: events.count, by: 2000) {
            let part = Array(events[start..<min(start + 2000, events.count)])
            send(Batch(events: part))
        }
    }

    private static func timesEvent(_ path: String) -> Event? {
        var st = stat()
        guard lstat(path, &st) == 0 else { return nil }
        func ns(_ t: timespec) -> Int64 { Int64(t.tv_sec) * 1_000_000_000 + Int64(t.tv_nsec) }
        return Event(p: path, a: ns(st.st_atimespec), m: ns(st.st_mtimespec))
    }

    // MARK: - Guest connection

    private func send(_ batch: Batch) {
        guard let data = try? encodeLengthPrefixed(batch) else { return }
        for _ in 0..<2 {
            guard let conn = openConnection() else { return }
            if (try? writeExactlyFD(conn.fileDescriptor, data: data)) != nil {
                return
            }
            closeConnection() // the guest restarted or the VM paused: reconnect once
        }
    }

    private func openConnection() -> VZVirtioSocketConnection? {
        if let c = connection { return c }
        guard let device = deviceProvider(),
              let conn = connectVsockOnce(device: device, port: controlPort, timeout: 2) else {
            return nil
        }
        let fd = conn.fileDescriptor
        guard let req = try? encodeLengthPrefixed(ControlRequest(cmd: "fs_events", args: nil)),
              (try? writeExactlyFD(fd, data: req)) != nil,
              let resp = try? decodeLengthPrefixedFD(ControlResponse.self, fd: fd),
              resp.status == "ok" else {
            // An older guest without fs_events: nothing to forward to.
            conn.close()
            return nil
        }
        connection = conn
        return conn
    }

    private func closeConnection() {
        connection?.close()
        connection = nil
    }
}
