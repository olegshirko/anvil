import XCTest
@testable import vz_runner

final class VMLifecycleManagerTests: XCTestCase {
    func testParseBootArgsMinimal() {
        let args = parseBootArgs(["--kernel", "/k", "--initrd", "/i"])
        XCTAssertNotNil(args)
        XCTAssertEqual(args?.kernelPath, "/k")
        XCTAssertEqual(args?.initrdPath, "/i")
        XCTAssertEqual(args?.memoryGiB, 2, "default RAM")
        XCTAssertEqual(args?.cpuCount, 2, "default CPUs")
        XCTAssertFalse(args?.useAgent ?? true)
        XCTAssertFalse(args?.debug ?? true)
    }

    func testParseBootArgsFull() {
        let args = parseBootArgs([
            "--kernel", "/k", "--initrd", "/i", "--agent", "--fresh",
            "--share", "/Users/oleg/proj", "--mount-tag", "custom",
            "--console", "/tmp/console.log", "--containerd-disk", "/tmp/disk.img",
            "--cpus", "8", "--memory", "4", "--debug",
        ])
        XCTAssertEqual(args?.sharePath, "/Users/oleg/proj")
        XCTAssertEqual(args?.mountTag, "custom")
        XCTAssertEqual(args?.cpuCount, 8)
        XCTAssertEqual(args?.memoryGiB, 4)
        XCTAssertEqual(args?.useAgent, true)
        XCTAssertEqual(args?.fresh, true)
        XCTAssertEqual(args?.debug, true)
    }

    func testParseBootArgsRejectsMissingPieces() {
        XCTAssertNil(parseBootArgs([]))
        XCTAssertNil(parseBootArgs(["--kernel", "/k"]), "initrd is required")
        XCTAssertNil(parseBootArgs(["--initrd", "/i"]), "kernel is required")
        // --help and unknown flags both bail out with the usage message.
        XCTAssertNil(parseBootArgs(["--kernel", "/k", "--initrd", "/i", "--help"]))
    }

    func testParseBootArgsGarbageNumbersFallBackToDefaults() {
        let args = parseBootArgs(["--kernel", "/k", "--initrd", "/i", "--cpus", "x", "--memory", "y"])
        XCTAssertEqual(args?.cpuCount, 2)
        XCTAssertEqual(args?.memoryGiB, 2)
    }

    func testBootPhaseTimerSmoke() {
        // mark() is fire-and-forget logging; make sure rapid + concurrent
        // marks don't crash or deadlock (it takes a lock per call).
        let timer = BootPhaseTimer()
        let group = DispatchGroup()
        for i in 0..<50 {
            group.enter()
            DispatchQueue.global().async {
                timer.mark("phase\(i)")
                group.leave()
            }
        }
        XCTAssertEqual(group.wait(timeout: .now() + 5), .success)
    }
}
