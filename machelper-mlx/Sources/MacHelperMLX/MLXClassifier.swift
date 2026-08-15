import Foundation
import MLXLLM
import MLXLMCommon

/// Classification against a local MLX text model.
///
/// The MLX path has no equivalent of Foundation Models' guided generation, so
/// the constraint is enforced in two places instead of one: the prompt names
/// the permitted doctypes explicitly, and every answer is re-validated against
/// the catalog before it is emitted. An out-of-catalog answer becomes a
/// `classify_failed` error, never an invented category (constraint 8).
enum MLXClassifier {

    /// Longest excerpt sent to the model.
    static let defaultMaxChars = 4000

    /// Upper bound on generated tokens. The answer is a small JSON object; a
    /// runaway generation is a bug, not a longer answer.
    static let maxTokens = 320

    /// Loads the model from the local cache and classifies `text`.
    ///
    /// Everything here is `async` end to end. Do NOT reintroduce a
    /// `DispatchSemaphore` bridge around this call: MLX's loader hops back to
    /// the main actor, so blocking the main thread on a semaphore deadlocks the
    /// process. That bug is on record; the `@main` entry point is `async` for
    /// exactly this reason.
    static func classify(
        text: String,
        catalog: DocTypeCatalog,
        repo: String,
        maxChars: Int
    ) async throws -> ClassifyResponse {
        let directory = try ModelCache.resolve(repo: repo)

        let container: ModelContainer
        do {
            container = try await LLMModelFactory.shared.loadContainer(
                configuration: ModelConfiguration(directory: directory)
            )
        } catch {
            throw HelperError(
                .modelLoadFailed,
                "could not load \(repo) from \(directory.path): \(error.localizedDescription)"
            )
        }

        let excerpt = String(text.prefix(max(maxChars, 200)))
        let chat: [Chat.Message] = [
            .system(instructions(catalog: catalog)),
            .user(prompt(excerpt: excerpt)),
        ]

        let output: String
        do {
            output = try await container.perform { context in
                let input = try await context.processor.prepare(input: UserInput(chat: chat))
                var produced = 0
                let result = try MLXLMCommon.generate(
                    input: input,
                    parameters: GenerateParameters(temperature: 0.0),
                    context: context
                ) { tokens in
                    produced = tokens.count
                    return produced >= maxTokens ? .stop : .more
                }
                return result.output
            }
        } catch {
            throw HelperError(.classifyFailed, "MLX generation failed: \(error.localizedDescription)")
        }

        return try decode(output: output, catalog: catalog, repo: repo)
    }

    /// Fast availability check: does the weight directory exist and look
    /// complete? No model is loaded, so this stays in the millisecond range —
    /// the Go core calls it before every classify.
    static func probe(repo: String) -> ProbeResponse {
        ModelCache.check(repo: repo)
    }

    // MARK: - Decoding

    /// Parses the model's answer and validates it against the catalog.
    static func decode(output: String, catalog: DocTypeCatalog, repo: String) throws -> ClassifyResponse {
        guard let json = firstJSONObject(in: output) else {
            throw HelperError(
                .classifyFailed,
                "\(repo) did not return a JSON object (got \(String(output.prefix(200)).debugDescription))"
            )
        }
        guard let data = json.data(using: .utf8),
              let root = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw HelperError(.classifyFailed, "\(repo) returned malformed JSON: \(json.debugDescription)")
        }
        guard let raw = (root["doctype"] as? String)?.trimmingCharacters(in: .whitespacesAndNewlines),
              !raw.isEmpty else {
            throw HelperError(.classifyFailed, "\(repo) returned no doctype")
        }
        guard let doctype = catalog.canonical(raw), let category = catalog.category(for: doctype) else {
            throw HelperError(
                .classifyFailed,
                "\(repo) returned \(raw.debugDescription), which is not in the supplied doctype catalog"
            )
        }

        var confidence = 0.0
        if let number = root["confidence"] as? Double {
            confidence = number
        } else if let number = root["confidence"] as? Int {
            confidence = Double(number)
        } else if let string = root["confidence"] as? String, let number = Double(string) {
            confidence = number
        }
        // Some small models answer 0-100 instead of 0-1.
        if confidence > 1 { confidence /= 100 }
        confidence = min(max(confidence, 0), 1).rounded(toPlaces: 4)

        var fields: [String: String] = [:]
        if let raw = root["fields"] as? [String: Any] {
            for (key, value) in raw {
                let name = key.trimmingCharacters(in: .whitespaces)
                guard !name.isEmpty, let text = stringify(value) else { continue }
                fields[name] = text
            }
        }

        return ClassifyResponse(
            contract: contractVersion,
            engine: "mlx",
            doctype: doctype,
            category: category,
            confidence: confidence,
            fields: fields
        )
    }

    private static func stringify(_ value: Any) -> String? {
        // JSONSerialization returns NSNumber for both numbers and booleans, and
        // `as? Int` succeeds on a boolean NSNumber — so without this check
        // first, `false` stringifies as "0" and lands in the vault as a fact
        // that says the opposite of nothing. Identity-check CFBoolean instead.
        if CFGetTypeID(value as AnyObject) == CFBooleanGetTypeID() {
            return (value as? Bool) == true ? "true" : "false"
        }
        switch value {
        case let text as String:
            let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
            return trimmed.isEmpty ? nil : trimmed
        case let number as Int:
            return String(number)
        case let number as Double:
            return number == number.rounded() ? String(Int(number)) : String(number)
        default:
            return nil
        }
    }

    /// Extracts the first balanced `{...}` run from the model's output.
    ///
    /// Small instruction-tuned models like to wrap JSON in prose or a
    /// ```json fence; brace matching survives both, and string-awareness keeps
    /// a brace inside a quoted value from ending the object early.
    static func firstJSONObject(in text: String) -> String? {
        var depth = 0
        var start: String.Index?
        var inString = false
        var escaped = false

        for index in text.indices {
            let character = text[index]
            if inString {
                if escaped {
                    escaped = false
                } else if character == "\\" {
                    escaped = true
                } else if character == "\"" {
                    inString = false
                }
                continue
            }
            switch character {
            case "\"":
                inString = true
            case "{":
                if depth == 0 { start = index }
                depth += 1
            case "}":
                guard depth > 0 else { break }
                depth -= 1
                if depth == 0, let begin = start {
                    return String(text[begin...index])
                }
            default:
                break
            }
        }
        return nil
    }

    // MARK: - Prompting

    /// System prompt. Mirrors the Apple backend's wording so the two tiers
    /// behave alike, with the JSON shape spelled out because MLX cannot
    /// constrain the grammar.
    static func instructions(catalog: DocTypeCatalog) -> String {
        """
        You classify documents for a personal document vault. All processing is on-device.

        Choose exactly one document type from this list, and nothing else:
        \(catalog.names.joined(separator: ", "))

        Reply with a single JSON object and no other text, in exactly this shape:
        {"doctype": "<one of the list above>", "confidence": <0.0 to 1.0>, "fields": {"<name>": "<value>"}}

        Rules:
        - Pick the single best match. Never invent a type that is not in the list.
        - confidence is your own certainty from 0.0 to 1.0. Use a low value when the
          text is ambiguous, truncated or unreadable; the caller falls back to rules
          when confidence is low, so an honest low score is more useful than a guess.
        - fields holds at most a handful of short facts that are literally present in
          the text, such as amount, issuer, document_number, date or account. Copy
          values verbatim. Never guess a value, and return an empty object rather
          than an invented one.
        """
    }

    /// The per-document prompt.
    static func prompt(excerpt: String) -> String {
        """
        Classify the following document text.

        ---
        \(excerpt)
        ---
        """
    }
}
