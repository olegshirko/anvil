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
    case "push":
        cmdImagesPush(args: Array(args.dropFirst()))
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
      anvil images push <image:tag> [--arch arm64|amd64]

    Manage Docker image mirrors in the docker-mirror repo
    (\(dockerMirrorRepo)). The reconcile workflow pulls requested images from
    the registry and publishes image-<arch>.tar.zst to a release tagged
    <safe-name>-<tag>-<arch>; the guest-agent loads that release as a
    fallback when the registry is unreachable.

    Subcommands:
      list      List Docker images already mirrored in docker-mirror.
      check     Check whether an image is on Docker Hub and/or mirrored.
      request   Request a new mirror (creates a docker-mirror issue).
      push      Mirror a local image right now: docker save + zstd, published
                as the release `anvil images list` shows. Bypasses the
                reconcile workflow.

    Options:
      --docker <image:tag>   Image (check / request), e.g. postgres:15.5
      --arch <arm64|amd64>   Architecture for check (default: arm64)
      --platform <os/arch>   Platform for request (default: linux/arm64)
      --json                 Machine-readable output (check)

    list and request use the GitHub API: set GITHUB_TOKEN to avoid rate
    limits. Without a token, request prints the prefilled issue URL instead.
    """)
}

// cmdImagesPush publishes a local image to docker-mirror immediately:
// `docker save` the ref, compress to image-<arch>.tar.zst, and create/replace
// the GitHub release the guest-agent downloads from — no reconcile workflow
// round-trip. docker and zstd come from PATH; GITHUB_TOKEN (or gh auth)
// authorizes the release API.
func cmdImagesPush(args: [String]) {
    var image: String?
    var arch = "arm64"
    var it = args.makeIterator()
    while let a = it.next() {
        switch a {
        case "--arch", "-a":
            guard let v = it.next() else { fallthrough }
            arch = v
        case "--help", "-h":
            printImagesUsage()
            exit(0)
        default:
            if a.hasPrefix("-") {
                print("unknown images push option: \(a)")
                exit(1)
            }
            image = a
        }
    }
    guard let ref = image else {
        print("usage: anvil images push <image:tag> [--arch arm64|amd64]")
        exit(1)
    }
    guard arch == "arm64" || arch == "amd64" else {
        print("error: --arch must be arm64 or amd64 (only these are mirrored)")
        exit(1)
    }
    guard let (name, tag) = splitMirrorRef(ref) else {
        print("error: \(ref) is not mirrorable (digest refs and dangling colons are not)")
        exit(1)
    }

    // 1. docker save + zstd into a temp dir.
    let fm = FileManager.default
    let tmp = fm.temporaryDirectory.appendingPathComponent("anvil-mirror-push-\(getpid())")
    try? fm.createDirectory(at: tmp, withIntermediateDirectories: true)
    defer { try? fm.removeItem(at: tmp) }
    let tarPath = tmp.appendingPathComponent("image.tar")
    let zstPath = tmp.appendingPathComponent("image-\(arch).tar.zst")
    print("[push] docker save \(ref)...")
    runTool("/usr/bin/env", ["docker", "save", "-o", tarPath.path, ref])
    print("[push] compressing with zstd...")
    runTool("/usr/bin/env", ["zstd", "-q", "-f", "-11", "-o", zstPath.path, tarPath.path])

    // 2. Create (or reuse) the release, then upload the asset with clobber.
    let releaseTag = mirrorReleaseTag(image: name, tag: tag, arch: arch)
    let token = ProcessInfo.processInfo.environment["GITHUB_TOKEN"] ?? ""
    let auth = ghAuthArgs(token: token)
    defer { auth.cleanup() }

    print("[push] release \(releaseTag) in \(dockerMirrorRepo)...")
    let createBody: [String: Any] = [
        "tag_name": releaseTag,
        "name": "\(ref) (\(arch))",
        "body": "Docker image mirror of \(ref) for \(arch), published by `anvil images push`.",
    ]
    guard let data = try? JSONSerialization.data(withJSONObject: createBody) else {
        print("error: cannot encode release payload")
        exit(1)
    }
    let bodyFile = tmp.appendingPathComponent("release.json")
    try? data.write(to: bodyFile)

    var curlArgs = ["-sS", "-X", "POST",
                    "https://api.github.com/repos/\(dockerMirrorRepo)/releases",
                    "-H", "Accept: application/vnd.github+json",
                    "-H", "Content-Type: application/json",
                    "-d", "@\(bodyFile.path)",
                    "-w", "\n%{http_code}"]
    curlArgs.append(contentsOf: auth.args)
    let (out, _) = runCurl(curlArgs)
    var status = "0"
    if let nl = out.lastIndex(of: "\n") { status = String(out[out.index(after: nl)...]) }

    var releaseID: Int?
    if status == "201" {
        releaseID = releaseIDFromJSON(String(out[..<out.lastIndex(of: "\n")!]))
    } else {
        // 422 "already_exists": fetch the existing release's id by tag.
        var getArgs = ["-sS",
                       "https://api.github.com/repos/\(dockerMirrorRepo)/releases/tags/\(releaseTag)",
                       "-w", "\n%{http_code}"]
        getArgs.append(contentsOf: auth.args)
        let (getOut, _) = runCurl(getArgs)
        var getStatus = "0"
        if let nl = getOut.lastIndex(of: "\n") { getStatus = String(getOut[getOut.index(after: nl)...]) }
        guard getStatus == "200" else {
            print("error: cannot create release \(releaseTag) (HTTP \(status)) and cannot read it (HTTP \(getStatus)). Set GITHUB_TOKEN with repo access to \(dockerMirrorRepo).")
            exit(1)
        }
        releaseID = releaseIDFromJSON(String(getOut[..<getOut.lastIndex(of: "\n")!]))
        print("[push] release exists, replacing the asset")
    }
    guard let id = releaseID else {
        print("error: release \(releaseTag) has no id in the API response")
        exit(1)
    }

    print("[push] uploading image-\(arch).tar.zst...")
    var upArgs = ["-sS", "-X", "POST",
                  "https://uploads.github.com/repos/\(dockerMirrorRepo)/releases/\(id)/assets?name=image-\(arch).tar.zst",
                  "-H", "Content-Type: application/octet-stream",
                  "--data-binary", "@\(zstPath.path)",
                  "-w", "\n%{http_code}"]
    upArgs.append(contentsOf: auth.args)
    let (upOut, _) = runCurl(upArgs)
    var upStatus = "0"
    if let nl = upOut.lastIndex(of: "\n") { upStatus = String(upOut[upOut.index(after: nl)...]) }
    guard upStatus == "201" else {
        print("error: asset upload failed (HTTP \(upStatus)):")
        if let nl = upOut.lastIndex(of: "\n") {
            print(String(upOut[..<nl]))
        } else {
            print(upOut)
        }
        exit(1)
    }
    print("Mirrored \(ref) (\(arch)) as release \(releaseTag).")
    print("Force-load it anywhere without touching Docker Hub: docker pull mirror.anvil/\(ref)")
}

// releaseIDFromJSON extracts "id" from a GitHub release JSON body.
func releaseIDFromJSON(_ body: String) -> Int? {
    guard let data = body.data(using: .utf8),
          let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
        return nil
    }
    return obj["id"] as? Int
}

// runTool runs a PATH-resolved tool to completion; any failure exits.
func runTool(_ path: String, _ args: [String]) {
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: path)
    proc.arguments = args
    proc.standardOutput = FileHandle.nullDevice
    let errPipe = Pipe()
    proc.standardError = errPipe
    do {
        try proc.run()
    } catch {
        print("error: cannot run \(path.contains("env") ? args.first ?? path : path) — \(error)")
        exit(1)
    }
    proc.waitUntilExit()
    guard proc.terminationStatus == 0 else {
        let err = String(data: errPipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
        print("error: \(args.first ?? path) failed (\(proc.terminationStatus))\(err.isEmpty ? "" : ":\n" + err)")
        exit(1)
    }
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
func mirrorReleaseTag(image: String, tag: String, arch: String = "arm64") -> String {
    let safe = image.replacingOccurrences(of: "/", with: "-")
    return "\(safe)-\(tag)-\(arch)"
}

// splitMirrorRef splits an image ref into (image, tag) the way the mirror
// naming needs it: a ":" before the last "/" is a registry port, not a tag,
// and a missing tag means "latest" (guest-agent splitImageTag semantics).
func splitMirrorRef(_ ref: String) -> (image: String, tag: String)? {
    if ref.contains("@") { return nil } // digest refs are not mirrorable
    let slash = ref.lastIndex(of: "/")
    let colon = ref.lastIndex(of: ":")
    switch (colon, slash) {
    case (nil, _):
        return (ref, "latest")
    case (let c?, let s?):
        if s > c { return (ref, "latest") } // registry port, no tag
        let tag = String(ref[ref.index(after: c)...])
        if tag.isEmpty { return nil } // dangling colon ("alpine:")
        return (String(ref[..<c]), tag)
    case (let c?, nil):
        let tag = String(ref[ref.index(after: c)...])
        if tag.isEmpty { return nil }
        return (String(ref[..<c]), tag)
    }
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

    let auth = ghAuthArgs(token: token)
    defer { auth.cleanup() }

    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: "/usr/bin/curl")
    proc.arguments = [
        "-sS", "-X", "POST",
        "https://api.github.com/repos/\(dockerMirrorRepo)/issues",
        "-H", "Accept: application/vnd.github+json",
        "-H", "X-GitHub-Api-Version: 2022-11-28",
        "-H", "Content-Type: application/json",
        "-d", "@\(tmp.path)",
        "-w", "\n%{http_code}",
    ] + auth.args
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

// ghAuthArgs returns curl args carrying the Authorization header when a
// token is given, plus the cleanup for the config file it wrote. The token
// never appears in argv: `ps` exposes command lines to every process of
// the user while curl runs, so the header goes through a 0600 curl config
// file (-K) instead. An empty token means unauthenticated (rate-limited).
func ghAuthArgs(token: String) -> (args: [String], cleanup: () -> Void) {
    guard !token.isEmpty else { return ([], {}) }
    let cfg = FileManager.default.temporaryDirectory
        .appendingPathComponent("anvil-gh-auth-\(getpid()).cfg")
    FileManager.default.createFile(
        atPath: cfg.path,
        contents: Data("header = \"Authorization: Bearer \(token)\"\n".utf8),
        attributes: [.posixPermissions: 0o600])
    return (["-K", cfg.path], { try? FileManager.default.removeItem(atPath: cfg.path) })
}

// mirrorDockerReleases lists docker-mirror releases that carry an
// image-<arch>.tar.zst asset (the discriminator that separates Docker mirrors
// from qcow2 VM base images, which live under the "master" / v* tags).
func mirrorDockerReleases() -> [[String: Any]]? {
    let auth = ghAuthArgs(token: ProcessInfo.processInfo.environment["GITHUB_TOKEN"] ?? "")
    defer { auth.cleanup() }
    var args = ["-sS", "-H", "Accept: application/vnd.github+json"]
    args.append(contentsOf: auth.args)
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
