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
///
/// The `anyOf` includes `unclassified` (`DocTypeCatalog.choices`). Without it
/// the model cannot decline, so it answers with its nearest miss at the same
/// confidence it would report for a genuine match — measured on real documents,
/// business proposals came back as `contract` at 0.90 and design mockups as
/// `certificate` at 0.90, with nothing ever returning unclassified. A schema
/// that only permits real answers does not stop a wrong one; it guarantees it.
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
                return ProbeResponse(
                    contract: contractVersion, engine: "apple", available: true, reason: nil, reasonCode: nil)
            case .unavailable(let reason):
                return ProbeResponse(
                    contract: contractVersion,
                    engine: "apple",
                    available: false,
                    reason: describe(reason),
                    // The OS has the model; this Mac cannot use it right now.
                    reasonCode: .modelUnavailable
                )
            @unknown default:
                return ProbeResponse(
                    contract: contractVersion,
                    engine: "apple",
                    available: false,
                    reason: "unknown_availability",
                    reasonCode: .unknown
                )
            }
        }
        return ProbeResponse(
            contract: contractVersion,
            engine: "apple",
            available: false,
            reason: "requires macOS 26 or newer",
            reasonCode: .osUnsupported
        )
        #else
        return ProbeResponse(
            contract: contractVersion,
            engine: "apple",
            available: false,
            reason: "built without the FoundationModels framework",
            reasonCode: .osUnsupported
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
        let answer = raw.trimmingCharacters(in: .whitespaces)

        // The model declined. That is a successful classification of "I do not
        // know", not a failure: it is reported with an empty category and zero
        // confidence, which is what the Go core reads as a decline. Fields are
        // dropped too — facts pulled from a document whose kind is unknown are
        // the least trustworthy output this helper can produce.
        if DocTypeCatalog.isUnclassified(answer) {
            return ClassifyResponse(
                contract: contractVersion,
                engine: "apple",
                doctype: DocTypeCatalog.unclassified,
                category: "",
                confidence: 0,
                fields: [:]
            )
        }

        // Belt and braces: the schema already restricts the answer, but an
        // out-of-catalog string must never reach the vault (constraint 8).
        guard let doctype = catalog.canonical(answer),
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
            description: "The single document type that best describes the text, or \"unclassified\" when none of them does.",
            anyOf: catalog.choices
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
        \(catalog.choices.joined(separator: ", "))

        Rules:
        - Pick the single best match. Never invent a type that is not in the list.
        - "unclassified" means none of the other types describes this document. Choose
          it whenever no listed type genuinely fits, and prefer it over a near miss: a
          near miss files the document in the wrong place, where the mistake is
          invisible, while "unclassified" simply asks the user. A document is not a
          contract because it is business writing, not a certificate because it is
          decorative, and not a receipt because it lists items. If you find yourself
          stretching a type to fit, the answer is "unclassified".
        - When the document names its own type in its title or letterhead -- "Tax
          Invoice", "Statement of Account", "Boarding Pass", "Business Proposal" --
          that wording is the strongest evidence there is. Match it to the listed
          type that means the same thing, and do not override it with a type
          suggested only by a phrase in the body such as "amount due".
        - Prefer the most specific type that actually fits over a more general one.
        - confidence is your own certainty from 0.0 to 1.0. Reserve 0.8 and above for a
          document that plainly announces its own type. Use 0.5 or below when the text
          is ambiguous, truncated or unreadable, or when two types fit about equally
          well; the caller falls back to deterministic rules when confidence is low, so
          an honest low score is more useful than a guess. Report 0.0 with
          "unclassified".
        - fields holds at most a handful of short facts that are literally present in
          the text, such as amount, issuer, document_number, date or account. Copy
          values verbatim. Never guess a value, never write a placeholder such as
          "unknown", "n/a" or "none", and return no fields at all rather than an
          invented one.
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
