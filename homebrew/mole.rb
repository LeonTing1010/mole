class Mole < Formula
  desc "Multi-protocol TUN VPN client powered by sing-box"
  homepage "https://github.com/LeonTing1010/mole"
  version "0.3.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/LeonTing1010/mole/releases/download/v#{version}/mole-darwin-arm64.tar.gz"
      # sha256 "UPDATE_AFTER_RELEASE"
    else
      url "https://github.com/LeonTing1010/mole/releases/download/v#{version}/mole-darwin-amd64.tar.gz"
      # sha256 "UPDATE_AFTER_RELEASE"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/LeonTing1010/mole/releases/download/v#{version}/mole-linux-arm64.tar.gz"
      # sha256 "UPDATE_AFTER_RELEASE"
    else
      url "https://github.com/LeonTing1010/mole/releases/download/v#{version}/mole-linux-amd64.tar.gz"
      # sha256 "UPDATE_AFTER_RELEASE"
    end
  end

  def install
    bin.install "mole"
  end

  test do
    assert_match "mole #{version}", shell_output("#{bin}/mole --version")
  end
end
