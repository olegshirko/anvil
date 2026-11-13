import Foundation

/// `anvil restart --restore`: a restart cold-boots the VM, and, as with
/// Docker, only containers with a restart policy come back. After every
/// upgrade the rest (compose stacks without `restart:`, one-off databases)
/// had to be started again by hand. The running containers are recorded
/// before the stop and started again once the Docker API answers.
enum RestoreContainers {
    /// Survives a failed start: `anvil start --restore` picks it up.
    static let listFile = stateDir.appendingPathComponent("restore-containers.json")

    struct Entry: Codable {
        let id: String
        let name: String
        let created: Int
    }

    /// Records the running containers; false when the API did not answer
    /// (nothing recorded, the caller says so).
    static func record() -> Bool {
        guard let body = api("GET", "/containers/json"),
              let list = try? JSONSerialization.jsonObject(with: body) as? [[String: Any]] else {
            return false
        }
        let entries = parse(list)
        guard let data = try? JSONEncoder().encode(entries) else { return false }
        try? data.write(to: listFile, options: .atomic)
        print("[anvil] recorded \(entries.count) running container(s) to restore")
        return true
    }

    /// The entries of a GET /containers/json answer, oldest first: a
    /// database is usually created before the app that needs it.
    static func parse(_ list: [[String: Any]]) -> [Entry] {
        list.compactMap { c -> Entry? in
            guard let id = c["Id"] as? String else { return nil }
            let name = ((c["Names"] as? [String])?.first ?? id).trimmingCharacters(in: CharacterSet(charactersIn: "/"))
            let created = (c["Created"] as? Int) ?? Int((c["Created"] as? Double) ?? 0)
            return Entry(id: id, name: name, created: created)
        }.sorted { ($0.created, $0.name) < ($1.created, $1.name) }
    }

    /// Starts the recorded containers that are not running yet.
    static func restore() {
        guard let data = try? Data(contentsOf: listFile),
              let entries = try? JSONDecoder().decode([Entry].self, from: data) else {
            print("[anvil] --restore: no recorded containers")
            return
        }
        guard waitForAPI(timeout: 120) else {
            print("[anvil] --restore: the Docker API did not answer; the list stays in \(listFile.path)")
            return
        }
        var started = 0, running = 0, gone = 0
        var failed: [String] = []
        for e in entries {
            switch state(of: e.id) {
            case .missing:
                gone += 1
            case .running:
                running += 1
            case .stopped:
                let (code, msg) = apiStatus("POST", "/containers/\(e.id)/start")
                if code == 204 || code == 304 {
                    started += 1
                } else {
                    failed.append("\(e.name) (\(msg.isEmpty ? "HTTP \(code)" : msg))")
                }
            }
        }
        var summary = "[anvil] restored \(started) container(s)"
        if running > 0 { summary += ", \(running) already running" }
        if gone > 0 { summary += ", \(gone) removed meanwhile" }
        print(summary)
        if failed.isEmpty {
            try? FileManager.default.removeItem(at: listFile)
        } else {
            print("[anvil] could not start: \(failed.joined(separator: ", "))")
        }
    }

    enum State { case running, stopped, missing }

    static func state(of id: String) -> State {
        guard let body = api("GET", "/containers/\(id)/json"),
              let c = try? JSONSerialization.jsonObject(with: body) as? [String: Any] else {
            return .missing
        }
        let st = c["State"] as? [String: Any]
        return (st?["Running"] as? Bool) == true ? .running : .stopped
    }

    static func waitForAPI(timeout: TimeInterval) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if let body = api("GET", "/_ping"), String(data: body, encoding: .utf8) == "OK" {
                return true
            }
            Thread.sleep(forTimeInterval: 0.5)
        }
        return false
    }

    /// The body of a successful (2xx) request, else nil.
    static func api(_ method: String, _ path: String) -> Data? {
        let (code, _, body) = curl(method, path)
        return (200..<300).contains(code) ? body : nil
    }

    /// The status and, on failure, the API's error message.
    static func apiStatus(_ method: String, _ path: String) -> (Int, String) {
        let (code, msg, _) = curl(method, path)
        return (code, msg)
    }

    private static func curl(_ method: String, _ path: String) -> (Int, String, Data) {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/usr/bin/curl")
        proc.arguments = ["-s", "-m", "60", "-X", method, "--unix-socket", dockerSocketPath,
                          "-w", "\n%{http_code}", "http://localhost/v1.51\(path)"]
        let pipe = Pipe()
        proc.standardOutput = pipe
        proc.standardError = FileHandle.nullDevice
        guard (try? proc.run()) != nil else { return (0, "curl failed", Data()) }
        let out = pipe.fileHandleForReading.readDataToEndOfFile()
        proc.waitUntilExit()
        guard let nl = out.lastIndex(of: UInt8(ascii: "\n")) else { return (0, "", Data()) }
        let body = out[out.startIndex..<nl]
        let code = Int(String(decoding: out[(nl + 1)...], as: UTF8.self)) ?? 0
        var msg = ""
        if !(200..<300).contains(code),
           let obj = try? JSONSerialization.jsonObject(with: body) as? [String: Any] {
            msg = obj["message"] as? String ?? ""
        }
        return (code, msg, Data(body))
    }
}
