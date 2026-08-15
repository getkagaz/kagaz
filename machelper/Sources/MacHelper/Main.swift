import Foundation

/// `kagaz-machelper` — the macOS leaf utility behind Kagaz's OCR and
/// classification tiers.
///
/// It holds zero vault logic (constraint 1): it reads one file or one stdin
/// buffer, asks an Apple framework a question, prints one JSON object on stdout
/// and exits. Every outcome, success or failure, is a versioned JSON contract
/// so the Go core can decode a single stream and fall back cleanly.
@main
struct Main {

    /// A proper async entry point. The MLX helper documents why this matters;
    /// the same rule holds here — never bridge async work with a semaphore.
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

    private static func dispatch(_ argv: [String]) async throws {
        guard let first = argv.first else {
            printUsage()
            throw HelperError(.badUsage, "no subcommand given; expected ocr, classify or --probe")
        }

        if first == "--help" || first == "-h" || first == "help" {
            printUsage()
            return
        }
        if first == "--version" {
            emit(VersionResponse(contract: contractVersion, tool: "kagaz-machelper", version: helperVersion))
            return
        }

        switch first {
        case "ocr":
            try runOCR(Array(argv.dropFirst()))
        case "classify":
            try await runClassify(Array(argv.dropFirst()))
        default:
            // `--probe` is accepted bare (`kagaz-machelper --probe`) as well as
            // after a subcommand; the Go core calls the bare form on every
            // classify to decide which backend to use.
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
                throw HelperError(.badUsage, "expected --probe, ocr or classify")
            }
            try runProbe(args)
        }
    }

    // Option sets are per-subcommand so that a name belonging to another
    // subcommand is rejected rather than quietly ignored.
    private static let ocrValueOptions: Set<String> = ["langs", "dpi", "max-pages"]
    private static let ocrFlags: Set<String> = ["json"]
    private static let classifyValueOptions: Set<String> = ["backend", "doctypes", "max-chars"]
    private static let classifyFlags: Set<String> = ["json", "probe"]
    private static let probeValueOptions: Set<String> = ["backend"]
    private static let probeFlags: Set<String> = ["json", "probe"]

    // MARK: - Subcommands

    private static func runOCR(_ argv: [String]) throws {
        let args = try Arguments(
            argv,
            optionsTakingValue: Self.ocrValueOptions,
            booleanFlags: Self.ocrFlags
        )
        guard let path = args.positionals.first else {
            throw HelperError(.badUsage, "ocr requires a file path")
        }
        guard args.positionals.count == 1 else {
            throw HelperError(.badUsage, "ocr takes exactly one file path")
        }
        let dpi = Double(try args.intValue("dpi", default: Int(VisionOCR.defaultDPI)))
        let maxPages = try args.intValue("max-pages", default: VisionOCR.defaultMaxPages)
        emit(
            try VisionOCR.run(
                path: path,
                languages: args.listValue("langs"),
                dpi: dpi,
                maxPages: maxPages
            )
        )
    }

    private static func runClassify(_ argv: [String]) async throws {
        let args = try Arguments(
            argv,
            optionsTakingValue: Self.classifyValueOptions,
            booleanFlags: Self.classifyFlags
        )
        let backend = args.value("backend", default: "apple")

        if args.flag("probe") {
            try runProbe(args)
            return
        }
        guard backend == "apple" else {
            throw HelperError(
                .unknownBackend,
                backend == "mlx"
                    ? "the mlx backend lives in the separate kagaz-machelper-mlx binary"
                    : "unknown backend \(backend.debugDescription); this binary implements \"apple\""
            )
        }
        guard let spec = args.value("doctypes") else {
            throw HelperError(.badUsage, "classify requires --doctypes \"name:category,...\"")
        }
        let catalog = try DocTypeCatalog(spec: spec)
        let text = try readStdin()
        let maxChars = try args.intValue("max-chars", default: AppleClassifier.defaultMaxChars)
        emit(try await AppleClassifier.classify(text: text, catalog: catalog, maxChars: maxChars))
    }

    private static func runProbe(_ args: Arguments) throws {
        let backend = args.value("backend", default: "apple")
        guard backend == "apple" else {
            throw HelperError(
                .unknownBackend,
                "unknown backend \(backend.debugDescription); this binary probes \"apple\""
            )
        }
        emit(AppleClassifier.probe())
    }

    // MARK: - Input

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

    // MARK: - Usage

    private static func printUsage() {
        // Usage goes to stderr so that stdout stays a pure JSON stream.
        FileHandle.standardError.write(Data(usage.utf8))
    }

    private static let usage = """
    kagaz-machelper \(helperVersion) — on-device OCR and classification for Kagaz.

    USAGE
      kagaz-machelper ocr <path> [--langs en-US,hi-IN] [--dpi 200] [--max-pages 200] [--json]
      kagaz-machelper classify --backend apple --doctypes "invoice:financial,..." [--max-chars N] [--json]
      kagaz-machelper --probe [--backend apple]
      kagaz-machelper --version

    NOTES
      classify reads the document text from stdin.
      ocr streams one page at a time; --max-pages caps the run (0 means no cap) and a
      capped run reports "truncated": true alongside "pages" and "total_pages".
      Output is always a single JSON object on stdout; --json is accepted for symmetry.
      Errors are JSON too: {"contract":1,"error":"<code>","message":"..."} with exit 1
      (exit 2 for usage errors).

    """
}

/// Human-facing version of the helper itself, distinct from the wire contract.
let helperVersion = "1.0.0"

/// `--version` payload.
///
/// The key is `tool`, not `engine`: `engine` means the inference backend
/// (`vision` / `apple` / `mlx`) everywhere else in the contract, and
/// overloading it with a binary name would make it undecodable.
struct VersionResponse: Encodable {
    let contract: Int
    let tool: String
    let version: String
}
