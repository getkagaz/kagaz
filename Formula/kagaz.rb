# frozen_string_literal: true

# Kagaz — local-first document vault manager for macOS.
#
# STATUS: released and installable — `brew install getkagaz/kagaz/kagaz`.
# The `url`/`sha256` here name the CURRENT release's tarball. On each v* tag,
# `release.yml` rewrites both, adds the `bottle do` block and pushes the
# result to the getkagaz/homebrew-kagaz tap, so this file in git trails the
# tap by one release until the next tag rewrites it. Edit it for anything
# except the version, url, sha256 and bottle block, which the workflow owns.
#
# Toolchain note: this formula deliberately does NOT `depends_on xcode: :build`.
# `machelper/` has no package dependencies and no macro plugin — guided
# generation is built at run time with `DynamicGenerationSchema`, not the
# `@Generable` macro — so it builds under plain Command Line Tools. See
# machelper/README.md ("Why not `@Generable`").
class Kagaz < Formula
  desc "Local-first document vault manager for macOS"
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

  depends_on "go" => :build
  # Apple silicon only, macOS 15 (Sequoia) floor (project constraint 10).
  depends_on arch: :arm64
  depends_on macos: :sequoia
  # pdftotext, the fast text-layer extraction tier.
  depends_on "poppler"

  def install
    ENV["GOTOOLCHAIN"] = "local"

    ldflags = "-s -w -X main.version=#{version}"
    system "go", "build", *std_go_args(ldflags: ldflags, output: bin/"kagaz"), "./cmd/kagaz"
    system "go", "build", *std_go_args(ldflags: ldflags, output: bin/"kagaz-mcp"), "./cmd/kagaz-mcp"

    # The Swift leaf helper (Vision OCR + Apple Foundation Models classify).
    # Builds under Command Line Tools or Xcode; no external SwiftPM packages,
    # so this step needs no network.
    cd "machelper" do
      system "swift", "build", "--disable-sandbox", "-c", "release"
      bin.install ".build/release/kagaz-machelper"
    end

    # `bin` is both the running executable's directory and on PATH, so the Go
    # core's helper lookup ($KAGAZ_MACHELPER → exe dir → PATH → /opt/homebrew/bin)
    # finds it without any configuration.
  end

  def caveats
    <<~EOS
      Get started:
        kagaz init
        kagaz doctor

      Optional tiers:
        - The Apple Foundation Models classifier additionally requires macOS 26.
          On macOS 15-25 Kagaz uses Apple Vision OCR plus the offline rules
          classifier, which is a complete vault manager on its own.
        - The MLX classifier is a separate, opt-in formula:
            brew install getkagaz/kagaz/kagaz-mlx
          It is not installed here on purpose: it pulls the whole MLX-Swift
          stack, and its weights are a multi-gigabyte `kagaz model pull`.

      There is no Homebrew Cask for the menu-bar app yet — it is a download
      from its own releases. It carries its own copy of this CLI, so the app
      does not need this formula; install this one to have `kagaz` in a
      terminal as well, and run `kagaz use` so both agree on one vault.
    EOS
  end

  test do
    # Functional test: build a real vault, then query it. No network.
    system bin/"kagaz", "init", "--root", testpath/"vault", "--demo"
    assert_path_exists testpath/"vault/vault.yaml"

    assert_match version.to_s, shell_output("#{bin}/kagaz --version")

    output = shell_output("#{bin}/kagaz --vault #{testpath}/vault/vault.yaml find --json")
    parsed = JSON.parse(output)
    refute_empty parsed, "kagaz find --json returned no results for a --demo vault"

    # The Swift helper's probe never loads a model and always exits 0; it is
    # the cheapest end-to-end check that the helper was built and is runnable.
    probe = JSON.parse(shell_output("#{bin}/kagaz-machelper --probe"))
    assert_equal 1, probe["contract"]
    assert_equal "apple", probe["engine"]

    # Actually run kagaz-mcp: merely asserting the file exists would pass for a
    # zero-byte file.
    assert_match version.to_s, shell_output("#{bin}/kagaz-mcp --version")
  end
end
