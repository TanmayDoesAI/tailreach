# Homebrew formula. This repository is its own tap:
#   brew tap tanmaydoesai/tailreach https://github.com/TanmayDoesAI/tailreach
#   brew install --HEAD tailreach
#   mydevices setup
class Tailreach < Formula
  desc "Your Mac's screen in any browser on your Tailscale tailnet"
  homepage "https://github.com/TanmayDoesAI/tailreach"
  license "Apache-2.0"
  head "https://github.com/TanmayDoesAI/tailreach.git", branch: "main"

  depends_on "go" => :build
  depends_on macos: :sonoma

  def install
    ENV["CGO_ENABLED"] = "0"
    # The binary keeps the name mydevices: macOS ties granted permissions to it.
    system "go", "build", *std_go_args(output: libexec/"mydevices", ldflags: "-s -w"), "."
    system "swiftc", "-O", "-swift-version", "5", *Dir["capture/*.swift"], "-o", libexec/"mydevices-capture"
    bin.install_symlink libexec/"mydevices"
  end

  def caveats
    <<~EOS
      Finish with:
        mydevices setup
      It signs tailreach with a local identity (so macOS remembers its permissions),
      starts it at login and lists what's left for you to do.
      Run it again after every `brew upgrade tailreach`.
    EOS
  end

  test do
    assert_match(/\d+\.\d+/, shell_output("#{bin}/mydevices version"))
  end
end
