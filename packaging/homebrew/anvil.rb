class Anvil < Formula
  desc "CLI tool that provides container runtimes on macOS and Linux"
  homepage "https://github.com/olegshirko/anvil"
  url "https://github.com/olegshirko/anvil/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "PLACEHOLDER_SHA256"
  license "MIT"
  head "https://github.com/olegshirko/anvil.git", branch: "main"

  depends_on "go" => :build
  depends_on "lima"
  depends_on "qemu"

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w"), "./cmd/anvil"
  end

  test do
    system "#{bin}/anvil", "version"
  end
end
