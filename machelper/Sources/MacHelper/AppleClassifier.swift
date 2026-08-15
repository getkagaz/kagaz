import Foundation

#if canImport(FoundationModels)
import FoundationModels
#endif

/// Classification against Apple's on-device Foundation Model.
///
/// Two independent gates guard this code:
///
///  * `#if canImport(FoundationModels)` — a compile-time gate, so the helper
///    still builds on an SDK without the framework;
///  * `@available(macOS 26, *)` — a run-time gate, so a binary built once and
///    shipped by Homebrew degrades to a structured error on macOS 15–25 rather
///    than trapping.
///
/// Guided generation is driven by `DynamicGenerationSchema`, built from the
/// `--doctypes` catalog at run time. That is a deliberate choice over the
/// `@Generable` macro: the catalog is user-configurable and unknown at compile
/// time, and the macro plugin ships only with full Xcode. The schema pins
/// `doctype` to an `anyOf` of the supplied names, so the model is structurally
/// incapable of inventing a category — constraint 8 is satisfied by the schema
/// itself and re-checked against the catalog below.
enum AppleClassifier {

    /// Longest prompt we send. The on-device model has a modest context; the
    /// first two thousand characters of a document carry the letterhead, the
    /// title and the key figures, which is what classification needs.
    static let defaultMaxChars = 2000

    /// Fast availability check for `--probe`. Queries the model's advertised
    /// availability only; it never loads or warms weights.
    static func probe() -> ProbeResponse {
        #if canImport(FoundationModels)
        if #available(macOS 26, *) {
            switch SystemLanguageModel.default.availability {
            case .available:
                return ProbeResponse(contract: contractVersion, engine: "apple", available: true, reason: nil)
            case .unavailable(let reason):
                return ProbeResponse(
                    contract: contractVersion,
                    engine: "apple",
                    available: false,
                    reason: describe(reason)
                )
            @unknown default:
                return ProbeResponse(
                    contract: contractVersion,
                    engine: "apple",
                    available: false,
                    reason: "unknown_availability"
                )
            }
        }
        return ProbeResponse(
            contract: contractVersion,
            engine: "apple",
            available: false,
            reason: "requires macOS 26 or newer"
        )
        #else
        return ProbeResponse(
            contract: contractVersion,
            engine: "apple",
            available: false,
            reason: "built without the FoundationModels framework"
        )
        #endif
    }

    /// Classifies `text` against `catalog`.
    static func classify(text: String, catalog: DocTypeCatalog, maxChars: Int) async throws -> ClassifyResponse {
        #if canImport(FoundationModels)
        guard #available(macOS 26, *) else {
            throw HelperError(.unsupportedOS, "the Apple Foundation Models backend requires macOS 26 or newer")
        }
        return try await run(text: text, catalog: catalog, maxChars: maxChars)
        #else
        throw HelperError(
            .backendUnavailable,
            "this build of kagaz-machelper has no FoundationModels support; rebuild on macOS 26 or use --backend mlx"
        )
        #endif
    }

    #if canImport(FoundationModels)

    @available(macOS 26, *)
    private static func run(text: String, catalog: DocTypeCatalog, maxChars: Int) async throws -> ClassifyResponse {
        switch SystemLanguageModel.default.availability {
        case .available:
            break
        case .unavailable(let reason):
            throw HelperError(.backendUnavailable, "Apple Foundation Models unavailable: \(describe(reason))")
        @unknown default:
            throw HelperError(.backendUnavailable, "Apple Foundation Models unavailable for an unknown reason")
        }

        let schema = try makeSchema(catalog: catalog)
        let session = LanguageModelSession(model: .default, instructions: instructions(catalog: catalog))
        let excerpt = String(text.prefix(max(maxChars, 200)))

        let response: LanguageModelSession.Response<GeneratedContent>
        do {
            response = try await session.respond(
                to: Prompt(prompt(excerpt: excerpt)),
                schema: schema,
                includeSchemaInPrompt: true,
                options: GenerationOptions(temperature: 0.0)
            )
        } catch {
            throw HelperError(.classifyFailed, "Foundation Models generation failed: \(error.localizedDescription)")
        }

        return try decode(content: response.content, catalog: catalog)
    }

    @available(macOS 26, *)
    private static func decode(content: GeneratedContent, catalog: DocTypeCatalog) throws -> ClassifyResponse {
        guard let raw = try? content.value(String.self, forProperty: "doctype"),
              !raw.trimmingCharacters(in: .whitespaces).isEmpty else {
            throw HelperError(.classifyFailed, "the model returned no doctype")
        }
        // Belt and braces: the schema already restricts the answer, but an
        // out-of-catalog string must never reach the vault (constraint 8).
        guard let doctype = catalog.canonical(raw.trimmingCharacters(in: .whitespaces)),
              let category = catalog.category(for: doctype) else {
            throw HelperError(.classifyFailed, "the model returned \(raw.debugDescription), which is not in the supplied doctype catalog")
        }

        var confidence = (try? content.value(Double.self, forProperty: "confidence")) ?? 0
        confidence = min(max(confidence, 0), 1).rounded(toPlaces: 4)

        return ClassifyResponse(
            contract: contractVersion,
            engine: "apple",
            doctype: doctype,
            category: category,
            confidence: confidence,
            fields: extractFields(content)
        )
    }

    @available(macOS 26, *)
    private static func extractFields(_ content: GeneratedContent) -> [String: String] {
        var fields: [String: String] = [:]
        guard let list = try? content.value([GeneratedContent].self, forProperty: "fields") else {
            return fields
        }
        for entry in list {
            guard let key = try? entry.value(String.self, forProperty: "key"),
                  let value = try? entry.value(String.self, forProperty: "value") else { continue }
            let trimmedKey = key.trimmingCharacters(in: .whitespaces)
            let trimmedValue = value.trimmingCharacters(in: .whitespaces)
            if trimmedKey.isEmpty || trimmedValue.isEmpty { continue }
            fields[trimmedKey] = trimmedValue
        }
        return fields
    }

    /// Builds the guided-generation schema for the supplied catalog.
    @available(macOS 26, *)
    private static func makeSchema(catalog: DocTypeCatalog) throws -> GenerationSchema {
        let doctype = DynamicGenerationSchema(
            name: "DocType",
            description: "The single document type that best describes the text.",
            anyOf: catalog.names
        )
        let field = DynamicGenerationSchema(
            name: "Field",
            description: "One extracted fact from the document.",
            properties: [
                DynamicGenerationSchema.Property(
                    name: "key",
                    description: "Short snake_case field name, e.g. amount, issuer, document_number.",
                    schema: DynamicGenerationSchema(type: String.self)
                ),
                DynamicGenerationSchema.Property(
                    name: "value",
                    description: "The value exactly as it appears in the document.",
                    schema: DynamicGenerationSchema(type: String.self)
                ),
            ]
        )
        let fields = DynamicGenerationSchema(arrayOf: field, minimumElements: 0, maximumElements: 8)
        let root = DynamicGenerationSchema(
            name: "Classification",
            description: "The classification of a single document.",
            properties: [
                DynamicGenerationSchema.Property(
                    name: "doctype",
                    description: "The chosen document type.",
                    schema: doctype
                ),
                DynamicGenerationSchema.Property(
                    name: "confidence",
                    description: "How certain the choice is, from 0.0 to 1.0.",
                    schema: DynamicGenerationSchema(type: Double.self)
                ),
                DynamicGenerationSchema.Property(
                    name: "fields",
                    description: "Facts worth indexing, or an empty list.",
                    schema: fields,
                    isOptional: true
                ),
            ]
        )
        do {
            return try GenerationSchema(root: root, dependencies: [field])
        } catch {
            throw HelperError(.internalError, "could not build the generation schema: \(error.localizedDescription)")
        }
    }

    @available(macOS 26, *)
    private static func describe(_ reason: SystemLanguageModel.Availability.UnavailableReason) -> String {
        switch reason {
        case .deviceNotEligible:
            return "device_not_eligible"
        case .appleIntelligenceNotEnabled:
            return "apple_intelligence_not_enabled"
        case .modelNotReady:
            return "model_not_ready"
        @unknown default:
            return "unavailable"
        }
    }
    #endif

    /// Shared with the MLX helper in spirit, not in code: the two packages are
    /// independent, so the wording lives in each.
    static func instructions(catalog: DocTypeCatalog) -> String {
        """
        You classify documents for a personal document vault. All processing is on-device.

        Choose exactly one document type from this list, and nothing else:
        \(catalog.names.joined(separator: ", "))

        Rules:
        - Pick the single best match. Never invent a type that is not in the list.
        - confidence is your own certainty from 0.0 to 1.0. Use a low value when the
          text is ambiguous, truncated or unreadable; the caller falls back to rules
          when confidence is low, so an honest low score is more useful than a guess.
        - fields holds at most a handful of short facts that are literally present in
          the text, such as amount, issuer, document_number, date or account. Copy
          values verbatim. Never guess a value, and return no fields at all rather
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
