import Foundation

enum ControlClient {
    static func status() {
        do {
            let resp = try sendUnix(request: ControlRequest(cmd: "health", args: nil))
            print("[anvil] VM status=\(resp.status ?? "unknown")")
        } catch {
            print("[anvil] status failed: \(error)")
            exit(1)
        }
    }

    static func exec(command: [String]) {
        // Avoid buffering when stdout is redirected to a pipe/file.
        setbuf(stdout, nil)
        do {
            let resp = try sendUnix(request: ControlRequest(cmd: "exec", args: command))
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
                print("[anvil] error: \(error)")
                exit(1)
            }
        } catch {
            print("[anvil] exec failed: \(error)")
            exit(1)
        }
    }

    // MARK: - Private

    private static func sendUnix(request: ControlRequest) throws -> ControlResponse {
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        _ = controlSocketPath.withCString {
            strncpy(&addr.sun_path.0, $0, MemoryLayout.size(ofValue: addr.sun_path) - 1)
        }

        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            throw NSError(domain: "anvil", code: 20, userInfo: [NSLocalizedDescriptionKey: "failed to create socket"])
        }
        setSocketNoSigPipe(fd)
        defer { close(fd) }

        let connected = withUnsafePointer(to: &addr) { ptr -> Bool in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
            }
        }
        guard connected else {
            throw NSError(domain: "anvil", code: 21, userInfo: [NSLocalizedDescriptionKey: String(cString: strerror(errno))])
        }

        let requestData = try encodeLengthPrefixed(request)
        try writeExactlyFD(fd, data: requestData)

        guard let lengthData = readExactlyFD(fd, count: MemoryLayout<UInt32>.size) else {
            throw NSError(domain: "anvil", code: 24, userInfo: [NSLocalizedDescriptionKey: "unexpected EOF reading length"])
        }
        let length = UInt32(bigEndian: lengthData.withUnsafeBytes { $0.load(as: UInt32.self) })
        guard length <= 16 * 1024 * 1024 else {
            throw NSError(domain: "anvil", code: 25, userInfo: [NSLocalizedDescriptionKey: "response too large"])
        }
        guard let body = readExactlyFD(fd, count: Int(length)) else {
            throw NSError(domain: "anvil", code: 28, userInfo: [NSLocalizedDescriptionKey: "unexpected EOF reading body"])
        }

        return try JSONDecoder().decode(ControlResponse.self, from: body)
    }
}
