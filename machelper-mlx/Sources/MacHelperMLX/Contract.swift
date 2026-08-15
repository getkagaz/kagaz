import Foundation

// This file mirrors machelper/Sources/MacHelper/Contract.swift on purpose.
// The two packages are deliberately independent — machelper must build with
// zero package dependencies for the default Homebrew formula, and this one
// pulls in the whole MLX graph — so the contract is duplicated rather than
// shared through a library. Keep the two files in step: the wire format is the
// same, and the Go core decodes both with one decoder.

/// The machelper JSON contract version. Bump only for a breaking change.
let contractVersion = 1

/// Machine-readable error codes emitted as `"error"` in the failure payload.
///
/// The Go core switches on these strings to decide whether to fall back to
/// rules-based classification and what to surface in `kagaz doctor`. Codes are
/// part of the contract: never rename one without bumping `contractVersion`.
enum ErrorCode: String {
    /// Arguments were missing, unknown, or malformed. Exit status 2.
    case badUsage = "bad_usage"
    /// `classify` was invoked with an empty stdin.
    case emptyInput = "empty_input"
    /// `--doctypes` was empty or could not be parsed.
    case invalidDoctypes = "invalid_doctypes"
    /// `--backend` named a backend this binary does not implement.
    case unknownBackend = "unknown_backend"
    /// The backend exists but is not usable right now.
    case backendUnavailable = "backend_unavailable"
    /// The model ran but its answer was unusable (empty, unparsable, or not in
    /// the supplied catalog).
    case classifyFailed = "classify_failed"
    /// The weight directory does not exist or is incomplete. The remedy is
    /// `kagaz model pull <repo>` — the helper itself never downloads anything.
    case modelNotFound = "model_not_found"
    /// The weights exist but MLX could not load them.
    case modelLoadFailed = "model_load_failed"
    /// Anything unforeseen. Always accompanied by a human message.
    case internalError = "internal_error"
}

/// A failure that is reportable on stdout as a structured contract payload.
struct HelperError: Error {
    let code: ErrorCode
    let message: String

    init(_ code: ErrorCode, _ message: String) {
        self.code = code
        self.message = message
    }

    /// Process exit status. Usage problems are 2, everything else is 1.
    var exitStatus: Int32 {
        code == .badUsage ? 2 : 1
    }
}

/// `classify` success payload.
struct ClassifyResponse: Encodable {
    let contract: Int
    let engine: String
    let doctype: String
    let category: String
    let confidence: Double
    let fields: [String: String]
}

/// `--probe` payload. Always exits 0 when the probe itself ran; `available`
/// carries the answer and `reason` explains a `false`.
struct ProbeResponse: Encodable {
    let contract: Int
    let engine: String
    let available: Bool
    let reason: String?
}

/// Failure payload. Written to stdout (not stderr) so the Go core can decode a
/// single stream, with a non-zero exit status.
struct ErrorResponse: Encodable {
    let contract: Int
    let error: String
    let message: String
}

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

/// Encodes and prints a payload on stdout, followed by a newline.
func emit<T: Encodable>(_ payload: T) {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
    guard let data = try? encoder.encode(payload),
          let text = String(data: data, encoding: .utf8) else {
        print(#"{"contract":\#(contractVersion),"error":"internal_error","message":"failed to encode response"}"#)
        return
    }
    print(text)
}

/// Prints the structured failure payload and terminates with its exit status.
func fail(_ error: HelperError) -> Never {
    emit(ErrorResponse(contract: contractVersion, error: error.code.rawValue, message: error.message))
    exit(error.exitStatus)
}

extension Double {
    /// Keeps the emitted JSON free of float noise like 0.9000000357627869.
    func rounded(toPlaces places: Int) -> Double {
        // Fully qualified: MLX re-exports a `pow` that would shadow this one.
        let factor = Foundation.pow(10.0, Double(places))
        return (self * factor).rounded() / factor
    }
}
