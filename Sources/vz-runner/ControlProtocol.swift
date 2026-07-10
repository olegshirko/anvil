import Foundation

let controlPort: UInt32 = 1024
let stateDir = FileManager.default.homeDirectoryForCurrentUser
    .appendingPathComponent(".anvil-vz", isDirectory: true)
let stateFile = stateDir.appendingPathComponent("run.json")
let controlSocketPath = stateDir.appendingPathComponent("control.sock").path

struct RunState: Codable {
    let pid: Int32
    let port: UInt32
}

struct ControlRequest: Codable {
    let cmd: String
    let args: [String]?
}

struct ControlResponse: Codable {
    let stdout: String?
    let stderr: String?
    let exitCode: Int?
    let error: String?
    let status: String?
}

func encodeLengthPrefixed<T: Encodable>(_ value: T) throws -> Data {
    let body = try JSONEncoder().encode(value)
    var length = UInt32(body.count).bigEndian
    var data = Data()
    data.append(Data(bytes: &length, count: MemoryLayout<UInt32>.size))
    data.append(body)
    return data
}

func decodeLengthPrefixed<T: Decodable>(_ type: T.Type, from stream: InputStream) throws -> T {
    let lengthData = try readExactly(stream, count: MemoryLayout<UInt32>.size)
    let length = UInt32(bigEndian: lengthData.withUnsafeBytes { $0.load(as: UInt32.self) })
    guard length <= 16 * 1024 * 1024 else {
        throw NSError(domain: "vz-runner", code: 1, userInfo: [NSLocalizedDescriptionKey: "response too large"])
    }
    let body = try readExactly(stream, count: Int(length))
    return try JSONDecoder().decode(type, from: body)
}

func readExactly(_ stream: InputStream, count: Int) throws -> Data {
    var buffer = Data(repeating: 0, count: count)
    var total = 0
    try buffer.withUnsafeMutableBytes { raw in
        guard let base = raw.baseAddress else { throw NSError(domain: "vz-runner", code: 2) }
        while total < count {
            let read = stream.read(base.advanced(by: total), maxLength: count - total)
            if read < 0, let error = stream.streamError {
                throw error
            }
            if read == 0 {
                throw NSError(domain: "vz-runner", code: 3, userInfo: [NSLocalizedDescriptionKey: "unexpected EOF"])
            }
            total += read
        }
    }
    return buffer
}

func saveState(_ state: RunState) {
    try? FileManager.default.createDirectory(at: stateDir, withIntermediateDirectories: true)
    if let data = try? JSONEncoder().encode(state) {
        try? data.write(to: stateFile)
    }
}

func loadState() -> RunState? {
    guard let data = try? Data(contentsOf: stateFile),
          let state = try? JSONDecoder().decode(RunState.self, from: data) else {
        return nil
    }
    if kill(state.pid, 0) != 0 {
        try? FileManager.default.removeItem(at: stateFile)
        try? FileManager.default.removeItem(atPath: controlSocketPath)
        return nil
    }
    return state
}

func writeAll(_ stream: OutputStream, data: Data) throws {
    var total = 0
    try data.withUnsafeBytes { raw in
        guard let base = raw.baseAddress else { throw NSError(domain: "vz-runner", code: 4) }
        while total < data.count {
            let written = stream.write(base.advanced(by: total), maxLength: data.count - total)
            if written < 0, let error = stream.streamError {
                throw error
            }
            if written == 0 {
                throw NSError(domain: "vz-runner", code: 5, userInfo: [NSLocalizedDescriptionKey: "write failed"])
            }
            total += written
        }
    }
}

// MARK: - Raw file-descriptor helpers used by the unix-socket control path

func readExactlyFD(_ fd: Int32, count: Int) -> Data? {
    var buffer = Data(repeating: 0, count: count)
    var total = 0
    let ok = buffer.withUnsafeMutableBytes { raw -> Bool in
        guard let base = raw.baseAddress else { return false }
        while total < count {
            let n = read(fd, base.advanced(by: total), count - total)
            if n <= 0 { return false }
            total += n
        }
        return true
    }
    return ok ? buffer : nil
}

func writeExactlyFD(_ fd: Int32, data: Data) throws {
    var total = 0
    try data.withUnsafeBytes { raw in
        guard let base = raw.baseAddress else { throw NSError(domain: "vz-runner", code: 6) }
        while total < data.count {
            let n = write(fd, base.advanced(by: total), data.count - total)
            if n < 0 { throw NSError(domain: "vz-runner", code: 7) }
            if n == 0 { throw NSError(domain: "vz-runner", code: 8) }
            total += n
        }
    }
}

func decodeLengthPrefixedFD<T: Decodable>(_ type: T.Type, fd: Int32) throws -> T {
    guard let lengthData = readExactlyFD(fd, count: MemoryLayout<UInt32>.size) else {
        throw NSError(domain: "vz-runner", code: 9, userInfo: [NSLocalizedDescriptionKey: "EOF reading length"])
    }
    let length = UInt32(bigEndian: lengthData.withUnsafeBytes { $0.load(as: UInt32.self) })
    guard length <= 16 * 1024 * 1024 else {
        throw NSError(domain: "vz-runner", code: 10, userInfo: [NSLocalizedDescriptionKey: "response too large"])
    }
    guard let body = readExactlyFD(fd, count: Int(length)) else {
        throw NSError(domain: "vz-runner", code: 11, userInfo: [NSLocalizedDescriptionKey: "EOF reading body"])
    }
    return try JSONDecoder().decode(type, from: body)
}
