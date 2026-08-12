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
      anvil images request --docker <image:tag> [--platform <os/arch>]

    Request that a Docker image be mirrored into the docker-mirror repo
    (\(dockerMirrorRepo)). The reconcile workflow pulls it from the registry
    and publishes image-arm64.tar.zst to a release tagged
    <safe-name>-<tag>-arm64; the guest-agent then loads that release as a
    fallback when the registry is unreachable.

    Options:
      --docker <image:tag>   Image to mirror, e.g. postgres:15.5
      --platform <os/arch>   Platform (default: linux/arm64)

    Requires GITHUB_TOKEN to create the issue via the API. Without a token the
    pre-filled issue URL is printed for manual submission.
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
