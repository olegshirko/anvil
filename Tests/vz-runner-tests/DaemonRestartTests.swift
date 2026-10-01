import XCTest
@testable import vz_runner

final class DaemonRestartTests: XCTestCase {
    func testRestartDelayExponentialUpToCap() {
        // attempt is 0-based: 1s, 2s, 4s, ..., capped at 60s.
        XCTAssertEqual(daemonRestartDelay(attempt: 0), 1)
        XCTAssertEqual(daemonRestartDelay(attempt: 1), 2)
        XCTAssertEqual(daemonRestartDelay(attempt: 2), 4)
        XCTAssertEqual(daemonRestartDelay(attempt: 5), 32)
        XCTAssertEqual(daemonRestartDelay(attempt: 6), 60, "1<<6=64 capped at 60")
        XCTAssertEqual(daemonRestartDelay(attempt: 100), 60, "long crash-loops stay at the cap")
    }

    func testRestartDelayNegativeAttemptSafe() {
        XCTAssertEqual(daemonRestartDelay(attempt: -3), 1, "negative attempt clamps to the base delay")
    }
}

final class StartConfigTests: XCTestCase {
    func testParseAnvilConfig() {
        let cfg = parseAnvilConfig("# c\nANVIL_MEMORY=4\nexport ANVIL_CPUS=\"3\"\n\nbad line\nANVIL_IDLE = '120'\n")
        XCTAssertEqual(cfg["ANVIL_MEMORY"], "4")
        XCTAssertEqual(cfg["ANVIL_CPUS"], "3")
        XCTAssertEqual(cfg["ANVIL_IDLE"], "120")
        XCTAssertNil(cfg["bad line"])
    }

    func testBootAssetsPreferPackageOverStateDir() {
        let brew = bootAssetCandidates(stateDir: "/s", stateBinDir: "/s/bin", brewAssets: "/b", projectRoot: nil)
        XCTAssertEqual(brew.kernels.first, "/b/vmlinuz-raw")
        XCTAssertEqual(brew.initrds.first, "/b/initramfs-containerd")
        let src = bootAssetCandidates(stateDir: "/s", stateBinDir: "/s/bin", brewAssets: nil, projectRoot: "/p")
        XCTAssertEqual(src.kernels.first, "/p/.download/alpine/vmlinuz-raw")
        XCTAssertEqual(src.initrds.first, "/p/.download/ubuntu/initramfs-containerd")
    }

}
