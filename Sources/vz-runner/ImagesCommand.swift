// Request a Docker image mirror in the docker-mirror repo. With GITHUB_TOKEN
// in the environment the issue is created via the API; without a token the
// pre-filled "new issue" URL is printed (and opened on macOS) so the user can
// submit it manually. The reconcile workflow in docker-mirror reads the hidden
// marker in the body and publishes image-arm64.tar.zst to a release tagged
// <safe-name>-<tag>-arm64, which the guest-agent loads as a pull fallback.

import Foundation

// docker-mirror repository: both the fallback image source (guest-agent) and
// the place where new mirror requests (issues) are filed.
let dockerMirrorRepo = "olegshirko/docker-mirror"

func cmdImages(args: [String]) {
    guard !args.isEmpty else {
        printImagesUsage()
        exit(1)
    }
    switch args[0] {
    case "request":
        cmdImagesRequest(args: Array(args.dropFirst()))
    case "list":
        cmdImagesList(args: Array(args.dropFirst()))
    case "check":
        cmdImagesCheck(args: Array(args.dropFirst()))
    case "--help", "-h":
        printImagesUsage()
        exit(0)
    default:
        print("unknown images subcommand: \(args[0])")
        printImagesUsage()
        exit(1)
    }
}

func printImagesUsage() {
    print("""
    Usage:
      anvil images list
      anvil images check --docker <image:tag> [--arch <arm64|amd64>] [--json]
      anvil images request --docker <image:tag> [--platform <os/arch>]

    Manage Docker image mirrors in the docker-mirror repo
    (\(dockerMirrorRepo)). The reconcile workflow pulls requested images from
    the registry and publishes image-<arch>.tar.zst to a release tagged
    <safe-name>-<tag>-<arch>; the guest-agent loads that release as a
    fallback when the registry is unreachable.

    Subcommands:
      list      List Docker images already mirrored in docker-mirror.
      check     Check whether an image is on Docker Hub and/or mirrored.
      request   Request a new mirror (creates a docker-mirror issue).

    Options:
      --docker <image:tag>   Image (check / request), e.g. postgres:15.5
      --arch <arm64|amd64>   Architecture for check (default: arm64)
      --platform <os/arch>   Platform for request (default: linux/arm64)
      --json                 Machine-readable output (check)

    list and request use the GitHub API: set GITHUB_TOKEN to avoid rate
    limits. Without a token, request prints the prefilled issue URL instead.
    """)
}

func cmdImagesRequest(args: [String]) {
    var docker = ""
    var platform = "linux/arm64"
    var i = 0
    while i < args.count {
        let a = args[i]
        switch a {
        case "--docker", "-d":
            i += 1
            if i < args.count { docker = args[i] }
        case "--platform", "-p":
            i += 1
            if i < args.count { platform = args[i] }
        case "--help", "-h":
            printImagesUsage()
            exit(0)
        default:
            print("unknown argument: \(a)")
            printImagesUsage()
            exit(1)
        }
        i += 1
    }

    guard !docker.isEmpty else {
        print("error: --docker <image:tag> is required")
        printImagesUsage()
        exit(1)
    }

    // Accept "image" alone (defaults to latest, matching Docker semantics and
    // the mirror release naming used by the guest-agent).
    let parts = docker.split(separator: ":", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
    let image = parts[0]
    let tag = parts.count > 1 ? parts[1] : "latest"

    let releaseTag = mirrorReleaseTag(image: image, tag: tag)
    let title = "[anvil] Request Docker image: \(image):\(tag) (\(platform))"
    let body = """
    <!-- anvil-request-image
    {"image":"\(image)","tag":"\(tag)","platform":"\(platform)"}
    -->

    **Request Type:** Docker image
    **Image:** \(image)
    **Tag:** \(tag)
    **Platform:** \(platform)

    This request was generated automatically by anvil. The reconcile workflow
    will process this issue and publish image-arm64.tar.zst to a release tagged
    \(releaseTag).
    """

    let token = ProcessInfo.processInfo.environment["GITHUB_TOKEN"] ?? ""
    if !token.isEmpty {
        createMirrorIssue(token: token, title: title, body: body)
    } else {
        printMirrorIssueURL(title: title, body: body)
    }
}

// safe-name: replace "/" with "-" (e.g. "library/nginx" -> "library-nginx").
// Must match guest-agent loadFromMirror and the reconcile workflow naming.
func mirrorReleaseTag(image: String, tag: String) -> String {
    let safe = image.replacingOccurrences(of: "/", with: "-")
    return "\(safe)-\(tag)-arm64"
}

// createMirrorIssue POSTs the issue via the GitHub API using curl (always
// available on macOS). The response body's html_url is reported on success.
func createMirrorIssue(token: String, title: String, body: String) {
    let payload: [String: Any] = ["title": title, "body": body]
    guard let data = try? JSONSerialization.data(withJSONObject: payload),
          let json = String(data: data, encoding: .utf8) else {
        print("error: cannot encode issue payload")
        exit(1)
    }

    let tmp = FileManager.default.temporaryDirectory.appendingPathComponent("anvil-issue-\(getpid()).json")
    do {
        try json.write(to: tmp, atomically: true, encoding: .utf8)
    } catch {
        print("error: cannot write payload: \(error)")
        exit(1)
    }
    defer { try? FileManager.default.removeItem(at: tmp) }

    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: "/usr/bin/curl")
    proc.arguments = [
        "-sS", "-X", "POST",
        "https://api.github.com/repos/\(dockerMirrorRepo)/issues",
        "-H", "Accept: application/vnd.github+json",
        "-H", "Authorization: Bearer \(token)",
        "-H", "X-GitHub-Api-Version: 2022-11-28",
        "-H", "Content-Type: application/json",
        "-d", "@\(tmp.path)",
        "-w", "\n%{http_code}",
    ]
    let pipe = Pipe()
    proc.standardOutput = pipe
    proc.standardError = Pipe()
    do {
        try proc.run()
    } catch {
        print("error: cannot run curl: \(error)")
        exit(1)
    }
    let out = String(data: pipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
    proc.waitUntilExit()

    // The last line is the HTTP status from -w "\n%{http_code}".
    var status = "0"
    var responseBody = out
    if let nl = out.lastIndex(of: "\n") {
        status = String(out[out.index(after: nl)...])
        responseBody = String(out[..<nl])
    }

    if status == "201" {
        if let data = responseBody.data(using: .utf8),
           let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
           let htmlURL = obj["html_url"] as? String {
            print("Issue created: \(htmlURL)")
        } else {
            print("Issue created in \(dockerMirrorRepo)")
        }
        print("The reconcile workflow will publish the mirror shortly. Re-run your docker command; the guest-agent loads it as a fallback once the release asset appears.")
    } else {
        print("error creating issue (HTTP \(status)):")
        print(responseBody)
        exit(1)
    }
}

// printMirrorIssueURL is the no-token fallback: print (and best-effort open)
// a pre-filled "new issue" URL so the user can submit it manually.
func printMirrorIssueURL(title: String, body: String) {
    let url = "https://github.com/\(dockerMirrorRepo)/issues/new?title=\(percentEncode(title))&body=\(percentEncode(body))"
    print("GITHUB_TOKEN not set. Open this URL to create the issue manually:")
    print(url)
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: "/usr/bin/open")
    proc.arguments = [url]
    proc.standardError = FileHandle.nullDevice
    try? proc.run()
}

func percentEncode(_ s: String) -> String {
    return s.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? s
}

// MARK: - list / check

// runCurl returns curl's stdout for the given arguments.
func runCurl(_ args: [String]) -> (output: String, exitCode: Int32) {
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: "/usr/bin/curl")
    proc.arguments = args
    let pipe = Pipe()
    proc.standardOutput = pipe
    proc.standardError = FileHandle.nullDevice
    do {
        try proc.run()
    } catch {
        return ("", -1)
    }
    let out = String(data: pipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
    proc.waitUntilExit()
    return (out, proc.terminationStatus)
}

// ghAuthArgs returns the Authorization header args when GITHUB_TOKEN is set,
// otherwise an empty list (unauthenticated; subject to API rate limits).
func ghAuthArgs() -> [String] {
    let token = ProcessInfo.processInfo.environment["GITHUB_TOKEN"] ?? ""
    return token.isEmpty ? [] : ["-H", "Authorization: Bearer \(token)"]
}

// mirrorDockerReleases lists docker-mirror releases that carry an
// image-<arch>.tar.zst asset (the discriminator that separates Docker mirrors
// from qcow2 VM base images, which live under the "master" / v* tags).
func mirrorDockerReleases() -> [[String: Any]]? {
    var args = ["-sS", "-H", "Accept: application/vnd.github+json"]
    args.append(contentsOf: ghAuthArgs())
    args.append("https://api.github.com/repos/\(dockerMirrorRepo)/releases?per_page=100")
    let (out, _) = runCurl(args)
    guard let data = out.data(using: .utf8),
          let arr = try? JSONSerialization.jsonObject(with: data) as? [[String: Any]] else {
        return nil
    }
    return arr.filter { rel in
        let assets = rel["assets"] as? [[String: Any]] ?? []
        return assets.contains { ($0["name"] as? String ?? "").hasPrefix("image-") }
    }
}

// cmdImagesList prints every Docker image mirrored in docker-mirror. The
// release "name" field carries the human-readable "image:tag (arch)" form
// (set by the reconcile workflow), which avoids the lossy reverse-mapping of
// the slash-stripped release tag.
func cmdImagesList(args: [String]) {
    let asJSON = args.contains("--json")
    guard let releases = mirrorDockerReleases() else {
        print("error: cannot fetch releases from \(dockerMirrorRepo) (set GITHUB_TOKEN to avoid rate limits)")
        exit(1)
    }
    let sorted = releases.sorted { ($0["tag_name"] as? String ?? "") < ($1["tag_name"] as? String ?? "") }

    if asJSON {
        var rows: [[String: String]] = []
        for rel in sorted {
            rows.append(releaseRow(rel))
        }
        if let data = try? JSONSerialization.data(withJSONObject: rows, options: [.prettyPrinted]),
           let s = String(data: data, encoding: .utf8) {
            print(s)
        }
        return
    }

    print("Mirrored Docker images (\(dockerMirrorRepo)):")
    if sorted.isEmpty {
        print("  (none)")
        return
    }
    let names = sorted.map { releaseRow($0)["name"] ?? "" }
    let nameW = (names.map { $0.count }.max() ?? 0)
    for rel in sorted {
        let row = releaseRow(rel)
        let name = (row["name"] ?? "").padded(to: nameW)
        print("  \(name)  \(row["tag"] ?? "")  (\(row["size"] ?? "?"))")
    }
}

// releaseRow extracts a display row (name / tag / size) from a release object.
func releaseRow(_ rel: [String: Any]) -> [String: String] {
    var name = (rel["name"] as? String ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
    let tag = rel["tag_name"] as? String ?? ""
    if name.isEmpty { name = tag }
    var size = ""
    if let assets = rel["assets"] as? [[String: Any]] {
        for a in assets {
            let an = a["name"] as? String ?? ""
            if an.hasPrefix("image-") {
                let sz = (a["size"] as? Int64) ?? Int64((a["size"] as? Int) ?? 0)
                size = sz > 0 ? formatBytes(sz) : "?"
                break
            }
        }
    }
    return ["name": name, "tag": tag, "size": size]
}

// cmdImagesCheck reports whether an image is available on Docker Hub and/or
// mirrored in docker-mirror for the requested arch. Exits 1 only when the
// image is available nowhere (a hint to run `anvil images request`).
func cmdImagesCheck(args: [String]) {
    var docker = ""
    var arch = "arm64"
    var asJSON = false
    var i = 0
    while i < args.count {
        let a = args[i]
        switch a {
        case "--docker", "-d":
            i += 1
            if i < args.count { docker = args[i] }
        case "--arch", "-a":
            i += 1
            if i < args.count { arch = args[i] }
        case "--json":
            asJSON = true
        case "--help", "-h":
            printImagesUsage()
            exit(0)
        default:
            print("unknown argument: \(a)")
            printImagesUsage()
            exit(1)
        }
        i += 1
    }

    guard !docker.isEmpty else {
        print("error: --docker <image:tag> is required")
        printImagesUsage()
        exit(1)
    }
    let parts = docker.split(separator: ":", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
    let image = parts[0]
    let tag = parts.count > 1 ? parts[1] : "latest"

    let hubOK = checkDockerHub(image: image, tag: tag, arch: arch)
    let mirrorOK = checkMirror(image: image, tag: tag, arch: arch)
    let ref = "\(image):\(tag)"

    if asJSON {
        let rows: [[String: String]] = [
            ["type": "docker-hub", "name": ref, "arch": arch, "status": hubOK ? "available" : "not found"],
            ["type": "docker-mirror", "name": ref, "arch": arch, "status": mirrorOK ? "mirrored" : "not mirrored"],
        ]
        if let data = try? JSONSerialization.data(withJSONObject: rows, options: [.prettyPrinted]),
           let s = String(data: data, encoding: .utf8) {
            print(s)
        }
    } else {
        print("docker-hub: \(ref) (\(arch))")
        print("  Status: \(hubOK ? "✓ available" : "✗ not found")")
        print("docker-mirror: \(ref) (\(arch))")
        print("  Status: \(mirrorOK ? "✓ mirrored" : "✗ not mirrored")")
        if !hubOK && !mirrorOK {
            print("Not available anywhere. Run: anvil images request --docker \(ref)")
        }
    }
    if !hubOK && !mirrorOK { exit(1) }
}

// checkDockerHub queries the Docker Hub tag API. Unqualified images (e.g.
// "postgres") map to the "library/" namespace. When the tag exists the images
// array is checked for the requested architecture.
func checkDockerHub(image: String, tag: String, arch: String) -> Bool {
    let repo = image.contains("/") ? image : "library/\(image)"
    let url = "https://hub.docker.com/v2/repositories/\(repo)/tags/\(tag)"
    let (out, _) = runCurl(["-sS", "-w", "\n%{http_code}", url])
    guard let nl = out.lastIndex(of: "\n") else { return false }
    let status = out[out.index(after: nl)...].trimmingCharacters(in: .whitespacesAndNewlines)
    guard status == "200" else { return false }
    let body = out[..<nl]
    guard let data = String(body).data(using: .utf8),
          let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
          let images = obj["images"] as? [[String: Any]] else {
        return true
    }
    for img in images {
        let a = (img["architecture"] as? String ?? "")
        if a == arch { return true }
    }
    return false
}

// checkMirror probes the docker-mirror release asset URL with a 1-byte range
// GET (no API call, no GitHub rate limit) and treats any 2xx as "mirrored".
func checkMirror(image: String, tag: String, arch: String) -> Bool {
    let safe = image.replacingOccurrences(of: "/", with: "-")
    let url = "https://github.com/\(dockerMirrorRepo)/releases/download/\(safe)-\(tag)-\(arch)/image-\(arch).tar.zst"
    let (out, _) = runCurl(["-sS", "-L", "-o", "/dev/null", "-w", "%{http_code}", "--range", "0-0", url])
    return out.trimmingCharacters(in: .whitespacesAndNewlines).hasPrefix("2")
}

// MARK: - formatting helpers

func formatBytes(_ bytes: Int64) -> String {
    if bytes < 1024 { return "\(bytes) B" }
    var v = Double(bytes)
    let units = ["KiB", "MiB", "GiB", "TiB"]
    var i = -1
    while v >= 1024 && i < units.count - 1 { v /= 1024; i += 1 }
    return String(format: "%.1f %@", v, units[max(i, 0)])
}

extension String {
    func padded(to length: Int) -> String {
        if count >= length { return self }
        return self + String(repeating: " ", count: length - count)
    }
}
