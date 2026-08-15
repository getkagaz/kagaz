# frozen_string_literal: true

# Kagaz MLX classifier tier — opt-in, never on the default install path.
#
# STATUS: no release has been tagged and no bottle has ever been published.
# `url`/`sha256` name the tarball the first tagged release will produce.
#
# Toolchain note: this package compiles MLX's bundled C++ sources, so it needs
# a complete C++ toolchain (a full libc++ header set). Either Xcode or a
# healthy Command Line Tools install satisfies that, which is why this formula
# states the requirement rather than hard-depending on Xcode. Verify yours with:
#
#   printf '#include <cstdlib>\nint main(){}\n' | clang++ -x c++ -c - -o /dev/null
#
# A Command Line Tools install with a truncated
# /Library/Developer/CommandLineTools/usr/include/c++/v1 fails that check and
# will fail this build with "fatal error: 'cstdlib' file not found". Reinstall
# the Command Line Tools, or set DEVELOPER_DIR to an Xcode install.
class KagazMlx < Formula
  desc "Opt-in MLX classification tier for Kagaz"
  homepage "https://github.com/getkagaz/kagaz"
  url "https://github.com/getkagaz/kagaz/archive/refs/tags/v0.1.0.tar.gz"
  version "0.1.0"
  # Placeholder: filled in by .github/workflows/release.yml at tag time.
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "MIT"
  head "https://github.com/getkagaz/kagaz.git", branch: "main"

  livecheck do
    url :stable
    strategy :github_latest
  end

  # MLX needs a Metal GPU; macOS 15 (Sequoia) floor as everywhere else.
  depends_on arch: :arm64
  depends_on macos: :sequoia

  # Deliberately no `depends_on "kagaz"`: this formula only adds a binary and
  # must not disturb, reinstall or shadow an existing core install. It installs
  # `kagaz-machelper-mlx` and nothing else, so there is no file overlap with
  # `kagaz` (which installs `kagaz`, `kagaz-mcp` and `kagaz-machelper`).

  def install
    # Unlike machelper/, this package resolves SwiftPM dependencies (MLX,
    # swift-transformers) from GitHub, so the *build* needs network access.
    # The built binary never makes a network call: weights are read from an
    # already-populated cache, and only `kagaz model pull` ever downloads.
    cd "machelper-mlx" do
      system "swift", "build", "--disable-sandbox", "-c", "release"
      bin.install ".build/release/kagaz-machelper-mlx"
    end
  end

  def caveats
    <<~EOS
      kagaz-machelper-mlx is installed but has no model weights yet. Fetch them
      with the Kagaz CLI (the only command in the project that touches the
      network):

        kagaz model pull mlx-community/Qwen2.5-3B-Instruct-4bit

      Weights live in ~/Library/Application Support/kagaz/models/<hf-repo>/.
      Until they are present, the MLX tier reports itself unavailable and Kagaz
      falls back to the next classifier tier.

      Building from source requires a working C++ toolchain (MLX ships C++
      sources). Check it with:
        printf '#include <cstdlib>\\nint main(){}\\n' | clang++ -x c++ -c - -o /dev/null
    EOS
  end

  test do
    # --probe never loads a model and never downloads anything; it exits 0
    # whether or not weights are present and puts the answer in `available`.
    probe = JSON.parse(shell_output("#{bin}/kagaz-machelper-mlx --probe"))
    assert_equal 1, probe["contract"]
    assert_equal "mlx", probe["engine"]
    # No weights in the sandbox, so this must be a clean, structured "no".
    refute probe["available"], "probe claimed MLX weights exist in a clean sandbox"

    # `--version` reports the helper's own compile-time version, which is not
    # the formula version, so assert the contract fields rather than a number.
    ver = JSON.parse(shell_output("#{bin}/kagaz-machelper-mlx --version"))
    assert_equal "kagaz-machelper-mlx", ver["tool"]
    refute_empty ver["version"]
  end
end
