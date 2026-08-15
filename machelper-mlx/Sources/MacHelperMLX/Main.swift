import Foundation

/// `kagaz-machelper-mlx` — the opt-in MLX classification tier for Kagaz.
///
/// Same JSON contract as `kagaz-machelper classify`, different engine. It holds
/// zero vault logic (constraint 1): text in on stdin, one JSON object out on
/// stdout. It never downloads anything (constraint 2) — weights come from the
/// `kagaz model pull` cache and a missing cache is a structured
/// `model_not_found` error.
@main
struct Main {

    /// A real async entry point.
    ///
    /// This is load-bearing, not stylistic. An earlier version bridged the
    /// async MLX loader with a `DispatchSemaphore` on the main thread and
    /// deadlocked every run. Keep `main` async and `await` all the way down.
    static func main() async {
        let argv = Array(CommandLine.arguments.dropFirst())
        do {
            try await dispatch(argv)
        } catch let error as HelperError {
            fail(error)
        } catch {
            fail(HelperError(.internalError, error.localizedDescription))
        }
    }

    // Option sets are per-subcommand so that an unknown name is rejected
    // rather than quietly swallowed as a flag plus a stray positional.
    private static let classifyValueOptions: Set<String> = ["backend", "doctypes", "model", "max-chars"]
    private static let classifyFlags: Set<String> = ["json", "probe"]
    private static let probeValueOptions: Set<String> = ["backend", "model"]
    private static let probeFlags: Set<String> = ["json", "probe"]

    private static func dispatch(_ argv: [String]) async throws {
        guard let first = argv.first else {
            printUsage()
            throw HelperError(.badUsage, "no subcommand given; expected classify or --probe")
        }
        if first == "--help" || first == "-h" || first == "help" {
            printUsage()
            return
        }
        if first == "--version" {
            emit(VersionResponse(contract: contractVersion, tool: "kagaz-machelper-mlx", version: helperVersion))
            return
        }

        switch first {
        case "classify":
            try await runClassify(Array(argv.dropFirst()))
        default:
            guard first.hasPrefix("--") else {
                printUsage()
                throw HelperError(.badUsage, "unknown subcommand \(first.debugDescription)")
            }
            let args = try Arguments(
                argv,
                optionsTakingValue: Self.probeValueOptions,
                booleanFlags: Self.probeFlags
            )
            guard args.flag("probe") else {
                printUsage()
                throw HelperError(.badUsage, "expected --probe or classify")
            }
            try runProbe(args)
        }
    }

    private static func runClassify(_ argv: [String]) async throws {
        let args = try Arguments(
            argv,
            optionsTakingValue: Self.classifyValueOptions,
            booleanFlags: Self.classifyFlags
        )
        if args.flag("probe") {
            try runProbe(args)
            return
        }
        try checkBackend(args)

        guard let spec = args.value("doctypes") else {
            throw HelperError(.badUsage, "classify requires --doctypes \"name:category,...\"")
        }
        let catalog = try DocTypeCatalog(spec: spec)
        let repo = args.value("model", default: ModelCache.defaultRepo)
        let maxChars = try args.intValue("max-chars", default: MLXClassifier.defaultMaxChars)
        let text = try readStdin()

        emit(try await MLXClassifier.classify(text: text, catalog: catalog, repo: repo, maxChars: maxChars))
    }

    private static func runProbe(_ args: Arguments) throws {
        try checkBackend(args)
        let repo = args.value("model", default: ModelCache.defaultRepo)
        // Validate the repo id before probing so a malformed --model is a
        // bad_usage error rather than an "unavailable" that looks like a
        // missing download.
        _ = try ModelCache.directory(for: repo)
        emit(MLXClassifier.probe(repo: repo))
    }

    private static func checkBackend(_ args: Arguments) throws {
        let backend = args.value("backend", default: "mlx")
        guard backend == "mlx" else {
            throw HelperError(
                .unknownBackend,
                backend == "apple"
                    ? "the apple backend lives in the kagaz-machelper binary"
                    : "unknown backend \(backend.debugDescription); this binary implements \"mlx\""
            )
        }
    }

    private static func readStdin() throws -> String {
        let data = FileHandle.standardInput.readDataToEndOfFile()
        guard let text = String(data: data, encoding: .utf8) else {
            throw HelperError(.emptyInput, "stdin is not valid UTF-8")
        }
        guard !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw HelperError(.emptyInput, "classify expects document text on stdin, but stdin was empty")
        }
        return text
    }

    private static func printUsage() {
        // Usage goes to stderr so that stdout stays a pure JSON stream.
        FileHandle.standardError.write(Data(usage.utf8))
    }

    private static let usage = """
    kagaz-machelper-mlx \(helperVersion) — on-device MLX classification for Kagaz.

    USAGE
      kagaz-machelper-mlx classify --backend mlx --doctypes "invoice:financial,..." \\
          [--model mlx-community/Qwen2.5-3B-Instruct-4bit] [--max-chars N] [--json]
      kagaz-machelper-mlx --probe [--model <repo>]
      kagaz-machelper-mlx --version

    NOTES
      classify reads the document text from stdin.
      Weights are read from ~/Library/Application Support/kagaz/models/<repo>/ and are
      never downloaded here; populate the cache with `kagaz model pull <repo>`.
      Output is always a single JSON object on stdout; --json is accepted for symmetry.
      Errors are JSON too: {"contract":1,"error":"<code>","message":"..."} with exit 1
      (exit 2 for usage errors).

    """
}

/// Human-facing version of the helper itself, distinct from the wire contract.
let helperVersion = "1.0.0"
