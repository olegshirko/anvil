import Foundation

enum ControlClient {
    static func status() {
        do {
            let resp = try sendUnixWithAutoStart(request: ControlRequest(cmd: "health", args: nil))
            print("[vz-runner] VM status=\(resp.status ?? "unknown")")
        } catch {
            print("[vz-runner] status failed: \(error)")
            exit(1)
        }
    }

    static func exec(command: [String]) {
        // Avoid buffering when stdout is redirected to a pipe/file.
        setbuf(stdout, nil)
        do {
            let resp = try sendUnixWithAutoStart(request: ControlRequest(cmd: "exec", args: command))
            if let out = resp.stdout, !out.isEmpty {
                print(out, terminator: "")
            }
            if let err = resp.stderr, !err.isEmpty {
                FileHandle.standardError.write(Data(err.utf8))
            }
            if let code = resp.exitCode {
                exit(Int32(code))
            }
            if let error = resp.error {
                print("[vz-runner] error: \(error)")
                exit(1)
            }
        } catch {
            print("[vz-runner] exec failed: \(error)")
            exit(1)
        }
    }

    // MARK: - Private

    private static var launchedDaemonProcess: Process?

    private static func sendUnixWithAutoStart(request: ControlRequest) throws -> ControlResponse {
        do {
            return try sendUnix(request: request)
        } catch {
            // Internal sync requests must never spawn a new daemon; otherwise a
            // killed daemon's sync child can auto-start an orphan process.
            if ProcessInfo.processInfo.environment["ANVIL_NO_DAEMON_AUTOSTART"] == "1" {
                throw error
            }
            print("[vz-runner] control socket unreachable, starting daemon...")
            try startDaemonAndWaitForSocket()
            return try sendUnix(request: request)
        }
    }

    private static func sendUnix(request: ControlRequest) throws -> ControlResponse {
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        _ = controlSocketPath.withCString {
            strncpy(&addr.sun_path.0, $0, MemoryLayout.size(ofValue: addr.sun_path) - 1)
        }

        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            throw NSError(domain: "vz-runner", code: 20, userInfo: [NSLocalizedDescriptionKey: "failed to create socket"])
        }
        setSocketNoSigPipe(fd)
        defer { close(fd) }

        let connected = withUnsafePointer(to: &addr) { ptr -> Bool in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
            }
        }
        guard connected else {
            throw NSError(domain: "vz-runner", code: 21, userInfo: [NSLocalizedDescriptionKey: String(cString: strerror(errno))])
        }

        let requestData = try encodeLengthPrefixed(request)
        try writeExactlyFD(fd, data: requestData)

        guard let lengthData = readExactlyFD(fd, count: MemoryLayout<UInt32>.size) else {
            throw NSError(domain: "vz-runner", code: 24, userInfo: [NSLocalizedDescriptionKey: "unexpected EOF reading length"])
        }
        let length = UInt32(bigEndian: lengthData.withUnsafeBytes { $0.load(as: UInt32.self) })
        guard length <= 16 * 1024 * 1024 else {
            throw NSError(domain: "vz-runner", code: 25, userInfo: [NSLocalizedDescriptionKey: "response too large"])
        }
        guard let body = readExactlyFD(fd, count: Int(length)) else {
            throw NSError(domain: "vz-runner", code: 28, userInfo: [NSLocalizedDescriptionKey: "unexpected EOF reading body"])
        }

        return try JSONDecoder().decode(ControlResponse.self, from: body)
    }

    private static func startDaemonAndWaitForSocket() throws {
        guard let executableURL = runnerExecutableURL() else {
            throw NSError(domain: "vz-runner", code: 40, userInfo: [NSLocalizedDescriptionKey: "cannot locate vz-runner executable"])
        }

        try? FileManager.default.createDirectory(at: stateDir, withIntermediateDirectories: true)
        let logURL = stateDir.appendingPathComponent("daemon.log")
        FileManager.default.createFile(atPath: logURL.path, contents: nil, attributes: nil)

        let process = Process()
        process.executableURL = executableURL
        process.arguments = ["daemon"]
        guard let logHandle = FileHandle(forWritingAtPath: logURL.path) else {
            throw NSError(domain: "vz-runner", code: 42, userInfo: [NSLocalizedDescriptionKey: "failed to open daemon log"])
        }
        process.standardOutput = logHandle
        process.standardError = logHandle
        process.terminationHandler = { proc in
            if proc.terminationStatus != 0 {
                print("[vz-runner] daemon exited with status \(proc.terminationStatus)")
            }
            launchedDaemonProcess = nil
        }
        launchedDaemonProcess = process
        try process.run()

        // Wait up to 10 seconds for the control socket to appear and accept connections.
        let deadline = Date().addingTimeInterval(10)
        while Date() < deadline {
            if canConnectToControlSocket() {
                return
            }
            Thread.sleep(forTimeInterval: 0.2)
        }

        throw NSError(domain: "vz-runner", code: 41, userInfo: [NSLocalizedDescriptionKey: "daemon did not publish control socket in time"])
    }

    private static func canConnectToControlSocket() -> Bool {
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        _ = controlSocketPath.withCString {
            strncpy(&addr.sun_path.0, $0, MemoryLayout.size(ofValue: addr.sun_path) - 1)
        }

        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return false }
        defer { close(fd) }

        let connected = withUnsafePointer(to: &addr) { ptr -> Bool in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
            }
        }
        return connected
    }

    private static func runnerExecutableURL() -> URL? {
        if let url = Bundle.main.executableURL {
            return url
        }
        let path = CommandLine.arguments[0]
        guard !path.isEmpty else { return nil }
        if path.hasPrefix("/") {
            return URL(fileURLWithPath: path)
        }
        return URL(fileURLWithFileSystemRepresentation: path, isDirectory: false, relativeTo: URL(fileURLWithPath: FileManager.default.currentDirectoryPath))
    }
}
