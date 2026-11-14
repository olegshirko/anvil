import XCTest
@testable import vz_runner

final class MirrorNamingTests: XCTestCase {
    // The release-tag and asset naming must agree with the guest-agent's
    // mirrorReleaseAsset and the reconcile workflow: safe-name replaces "/"
    // with "-", the tag follows, then the arch.
    func testReleaseTagArchs() {
        XCTAssertEqual(mirrorReleaseTag(image: "postgres", tag: "15.5"), "postgres-15.5-arm64")
        XCTAssertEqual(mirrorReleaseTag(image: "postgres", tag: "15.5", arch: "arm64"), "postgres-15.5-arm64")
        XCTAssertEqual(mirrorReleaseTag(image: "apache/kafka", tag: "4.1.1", arch: "amd64"), "apache-kafka-4.1.1-amd64")
        XCTAssertEqual(mirrorReleaseTag(image: "library/nginx", tag: "latest", arch: "arm64"), "library-nginx-latest-arm64")
    }

    // splitMirrorRef mirrors the guest-agent's splitImageTag semantics:
    // implicit :latest, registry ports are not tags, digests and dangling
    // colons are not mirrorable.
    func testSplitMirrorRef() {
        XCTAssertEqual(splitMirrorRef("postgres:15.5")?.image, "postgres")
        XCTAssertEqual(splitMirrorRef("postgres:15.5")?.tag, "15.5")
        XCTAssertEqual(splitMirrorRef("postgres")?.tag, "latest")
        XCTAssertEqual(splitMirrorRef("apache/kafka:4.1.1")?.image, "apache/kafka")
        XCTAssertEqual(splitMirrorRef("myreg.io:5000/img:1")?.image, "myreg.io:5000/img")
        XCTAssertEqual(splitMirrorRef("myreg.io:5000/img")?.tag, "latest")
        XCTAssertNil(splitMirrorRef("postgres@sha256:abc"))
        XCTAssertNil(splitMirrorRef("alpine:"))
    }
}
