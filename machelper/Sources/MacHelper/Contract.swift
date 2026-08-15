import Foundation

/// The machelper JSON contract version. Bump only for a breaking change; the
/// Go core (`internal/vaultkit/ocr`, `internal/vaultkit/classify`) refuses a
/// payload whose `contract` it does not know.
let contractVersion = 1

// MARK: - Error codes

/// Machine-readable error codes emitted as `"error"` in the failure payload.
///
/// The Go core switches on these strings to decide whether to fall back to
/// rules-based classification, to a different OCR engine, or to surface the
/// problem in `kagaz doctor`. Codes are part of the contract: never rename one
/// without bumping `contractVersion`.
enum ErrorCode: String {
    /// Arguments were missing, unknown, or malformed. Exit status 2.
    case badUsage = "bad_usage"
    /// The input path does not exist or is not readable.
    case fileNotFound = "file_not_found"
    /// The file exists but could not be decoded as an image or a PDF.
    case unsupportedFormat = "unsupported_format"
    /// A page could not be rasterised for recognition.
    case renderFailed = "render_failed"
    /// Vision returned an error while performing the request.
    case ocrFailed = "ocr_failed"
    /// Recognition succeeded but produced no text at all.
    case noText = "no_text"
    /// `classify` was invoked with an empty stdin.
    case emptyInput = "empty_input"
    /// `--doctypes` was empty or could not be parsed.
    case invalidDoctypes = "invalid_doctypes"
    /// `--backend` named a backend this binary does not implement.
    case unknownBackend = "unknown_backend"
    /// The OS is older than the backend requires (Apple backend needs macOS 26).
    case unsupportedOS = "unsupported_os"
    /// The backend exists but is not usable right now (Apple Intelligence off,
    /// device not eligible, model still downloading, weights not pulled).
    case backendUnavailable = "backend_unavailable"
    /// The model ran but its answer was unusable (empty, or not in the catalog).
    case classifyFailed = "classify_failed"
    /// The MLX weight directory does not exist. Run `kagaz model pull`.
    case modelNotFound = "model_not_found"
    /// The MLX weights exist but could not be loaded.
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

// MARK: - Payloads

/// One recognised text block. `bbox` is `[x, y, w, h]` in *normalised* page
/// coordinates (0...1) with the origin at the **top-left** of the page, which
/// is the orientation the Go core and the UI expect. Vision's native
/// bottom-left origin is converted before emission.
struct OCRBlock: Encodable {
    let text: String
    let bbox: [Double]
    let confidence: Double
    let page: Int
}

/// `ocr` success payload.
struct OCRResponse: Encodable {
    let contract: Int
    let engine: String
    let confidence: Double
    let blocks: [OCRBlock]
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

// MARK: - Emission

/// Encodes and prints a payload on stdout, followed by a newline.
func emit<T: Encodable>(_ payload: T) {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
    guard let data = try? encoder.encode(payload),
          let text = String(data: data, encoding: .utf8) else {
        // Last-resort hand-built payload; encoding a contract struct cannot
        // realistically fail, but the core must never see a truncated stream.
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
