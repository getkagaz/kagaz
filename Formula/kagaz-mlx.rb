# frozen_string_literal: true

# Kagaz MLX classifier tier — opt-in, never on the default install path.
#
# STATUS: no release has been tagged and no bottle has ever been published.
# `url`/`sha256` name the tarball the first tagged release will produce, so
# this formula is not installable yet in any form — the tap it would come
# from is empty until release.yml populates it. Build from source instead;
# see docs/installation.md.
#
# Toolchain note: this formula requires full Xcode, and the *base* `kagaz`
# formula deliberately does not. Two separate reasons converge here:
#
#  1. `xcrun metal` — the Metal shader compiler — ships only with Xcode, never
#     with the Command Line Tools. SwiftPM has no build rule for `.metal`
#     sources, so `swift build` alone links a binary whose shader library was
#     never compiled and which dies on its first MLX operation with
#     "MLX error: Failed to load the default metallib". Scripts/build-metallib.sh
#     is that missing step and it cannot run without Xcode. This is the hard
#     requirement.
#  2. MLX also compiles bundled C++ sources, which need a complete libc++
#     header set. Xcode supplies one; so does a healthy Command Line Tools
#     install, which is why this was previously stated as a caveat rather than
#     a dependency. Reason 1 supersedes that: nothing short of Xcode suffices.
#
# `machelper/` (the default formula) has no Metal, no macro plugin and no C++,
# and builds under plain Command Line Tools. Do not propagate this dependency
# to Formula/kagaz.rb.
class KagazMlx < Formula
  desc "Opt-in MLX classification tier for Kagaz"
  homepage "https://github.com/getkagaz/kagaz"
  url "https://github.com/getkagaz/kagaz/archive/refs/tags/v0.1.0.tar.gz"
  # Rewritten by .github/workflows/release.yml at tag time; the value
  # here is a placeholder until then.
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "MIT"
  head "https://github.com/getkagaz/kagaz.git", branch: "main"

  livecheck do
    url :stable
    strategy :github_latest
  end

  # Build-time only: `xcrun metal` compiles the shader library. A bottled
  # install pulls in no Xcode. 16.0 is the floor that ships a Metal compiler
  # targeting the macOS 15 SDK this package deploys against.
  depends_on xcode: ["16.0", :build]
  # MLX needs a Metal GPU; macOS 15 (Sequoia) floor as everywhere else.
  depends_on arch: :arm64
  depends_on macos: :sequoia

  # Deliberately no `depends_on "kagaz"`: this formula only adds files and must
  # not disturb, reinstall or shadow an existing core install. It installs
  # `kagaz-machelper-mlx` and `mlx.metallib` and nothing else, so there is no
  # file overlap with `kagaz` (which installs `kagaz`, `kagaz-mcp` and
  # `kagaz-machelper`).

  def install
    # Unlike machelper/, this package resolves SwiftPM dependencies (MLX,
    # swift-transformers) from GitHub, so the *build* needs network access.
    # The built binary never makes a network call: weights are read from an
    # already-populated cache, and only `kagaz model pull` ever downloads.
    cd "machelper-mlx" do
      system "swift", "build", "--disable-sandbox", "-c", "release"

      # SwiftPM has no build rule for `.metal`, so `swift build` above produced
      # a binary with no shader library. This compiles it with xcrun metal /
      # metallib against the mlx-swift checkout SwiftPM just resolved. Without
      # this step the install links, runs `--version`, and then dies on the
      # first MLX operation with "Failed to load the default metallib".
      # Two things are wrong with the default environment here, and both are
      # invisible until the formula is actually built rather than read.
      #
      # `depends_on xcode:` asserts Xcode is *installed*; it does not make the
      # build *use* it. Homebrew builds under whatever `xcode-select -p` points
      # at, which is /Library/Developer/CommandLineTools by default -- the
      # normal state of a Mac that also has Xcode -- and `xcrun metal` ships
      # only inside Xcode. So DEVELOPER_DIR has to be set.
      #
      # But setting it is not enough: Homebrew puts its own `xcrun` shim ahead
      # of /usr/bin, and that shim contains, verbatim,
      #   # Some build tools set DEVELOPER_DIR, so discard it
      #   unset DEVELOPER_DIR
      # for every invocation that is not --show-sdk-*. Setting DEVELOPER_DIR via
      # ENV[...], via env(1), or via HOMEBREW_DEVELOPER_DIR all fail identically
      # because of that line. Putting /usr/bin first is what reaches the real
      # xcrun, which honours DEVELOPER_DIR and finds metal in its cryptex mount.
      #
      # Without both, this fails with "the Metal compiler is not available"
      # three minutes into the build, for anyone who has not run
      # `sudo xcode-select -s`.
      system "/usr/bin/env", "DEVELOPER_DIR=#{MacOS::Xcode.prefix}",
             "PATH=/usr/bin:#{ENV["PATH"]}",
             "./Scripts/build-metallib.sh", "-c", "release"

      # BOTH files must land in the SAME directory. mlx's first lookup is
      # `<dir of the running binary>/mlx.metallib`, resolved via `dladdr`.
      # dladdr resolves through symlinks to the real file, so the pair lives in
      # libexec and only the executable is linked into bin: `brew audit
      # --strict` rejects a non-executable in bin, and mlx.metallib is data.
      # Keep them together wherever they go — splitting them makes the helper
      # die on its first MLX call with "Failed to load the default metallib".
      libexec.install ".build/release/kagaz-machelper-mlx"
      libexec.install ".build/release/mlx.metallib"
      bin.install_symlink libexec/"kagaz-machelper-mlx"
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

      Building this formula from source requires full Xcode, not just the
      Command Line Tools: MLX's Metal shader library is compiled with
      `xcrun metal`, which ships only inside Xcode. The base `kagaz` formula
      has no such requirement and is unaffected.

      mlx.metallib is installed alongside kagaz-machelper-mlx in libexec and
      must stay there; MLX loads it from the directory of the running binary,
      resolving through the symlink in bin.
    EOS
  end

  test do
    # The metallib must be installed next to the binary, or MLX cannot load a
    # shader and the helper dies on its first real operation.
    assert_path_exists libexec/"mlx.metallib"

    # --probe never loads a model and never downloads anything; it exits 0
    # whether or not weights are present and puts the answer in `available`.
    probe = JSON.parse(shell_output("#{bin}/kagaz-machelper-mlx --probe"))
    assert_equal 1, probe["contract"]
    assert_equal "mlx", probe["engine"]
    # REGRESSION GUARD. The probe checks two things: that the MLX runtime works
    # (Metal device + shader library) and that the weights are present. Only the
    # second is allowed to fail. A metallib complaint means `swift build`
    # shipped a binary with no compiled shaders again — the exact defect this
    # formula's build-metallib.sh step exists to prevent.
    #
    # Both outcomes are legitimate, so the test accepts either. `brew test`
    # sandboxes the working directory, not $HOME, and the weights live in
    # ~/Library/Application Support/kagaz/models — so on any machine where
    # someone has actually run `kagaz model pull`, available is true. Asserting
    # otherwise made this test pass on CI and fail on precisely the machines
    # that use the MLX tier.
    if probe["available"]
      # Stronger than the branch below: the runtime answered at all, which it
      # cannot do without loading its shader library.
      assert_empty probe["reason"].to_s, "an available probe needs no reason"
    else
      reason = probe["reason"].to_s
      refute_empty reason, "an unavailable probe must always carry a reason"
      assert_match(/weight/i, reason)
      refute_match(/metallib|shader/i, reason)
    end

    # `--version` reports the helper's own compile-time version, which is not
    # the formula version, so assert the contract fields rather than a number.
    ver = JSON.parse(shell_output("#{bin}/kagaz-machelper-mlx --version"))
    assert_equal "kagaz-machelper-mlx", ver["tool"]
    refute_empty ver["version"]
  end
end
