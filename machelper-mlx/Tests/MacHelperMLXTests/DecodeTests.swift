import Testing

@testable import MacHelperMLX

/// Tests for the pure half of the MLX classifier: extracting the model's JSON
/// out of whatever prose it wrapped it in, and validating the result against
/// the doctype catalog.
///
/// This is deliberately the only tested surface. It is also the riskiest: none
/// of it runs during a live MLX generation's happy path in a way anyone would
/// notice, small instruction-tuned models produce all of the shapes below, and
/// the brace matcher is the kind of code that regresses silently.
private let catalog = try! DocTypeCatalog(
    spec: "invoice:financial,passport:identity,lease:property"
)

// MARK: - firstJSONObject

@Test("plain JSON object is returned as-is")
func plainObject() {
    let text = #"{"doctype": "invoice"}"#
    #expect(MLXClassifier.firstJSONObject(in: text) == text)
}

@Test("a ```json fence is stripped")
func fencedJSON() {
    let text = """
        Here you go:
        ```json
        {"doctype": "invoice", "confidence": 0.9}
        ```
        """
    #expect(MLXClassifier.firstJSONObject(in: text) == #"{"doctype": "invoice", "confidence": 0.9}"#)
}

@Test("prose before and after the object is ignored")
func proseWrapped() {
    let text = "I think this is an invoice. {\"doctype\": \"invoice\"} Hope that helps!"
    #expect(MLXClassifier.firstJSONObject(in: text) == #"{"doctype": "invoice"}"#)
}

@Test("a brace inside a string value does not end the object early")
func braceInsideString() {
    let text = #"{"doctype": "invoice", "fields": {"note": "amount } total {"}}"#
    // The naive matcher stops at the first '}' inside the note; the correct
    // one returns the whole object.
    #expect(MLXClassifier.firstJSONObject(in: text) == text)
}

@Test("an escaped quote does not end the string")
func escapedQuote() {
    let text = #"{"doctype": "invoice", "fields": {"note": "he said \"} }\" loudly"}}"#
    #expect(MLXClassifier.firstJSONObject(in: text) == text)
}

@Test("unbalanced braces yield nil rather than a truncated object")
func unbalancedBraces() {
    #expect(MLXClassifier.firstJSONObject(in: #"{"doctype": "invoice""#) == nil)
    #expect(MLXClassifier.firstJSONObject(in: "no json here at all") == nil)
    // A stray closing brace must not underflow the depth counter. The
    // `guard depth > 0 else { break }` in the matcher breaks the *switch*, not
    // the loop, so the real object that follows is still found — this pins
    // that behaviour, because reading it as a loop break is an easy mistake.
    #expect(MLXClassifier.firstJSONObject(in: #"} {"doctype": "lease"}"#) == #"{"doctype": "lease"}"#)
}

@Test("the first of several objects wins")
func firstOfSeveral() {
    let text = #"{"doctype": "invoice"} {"doctype": "lease"}"#
    #expect(MLXClassifier.firstJSONObject(in: text) == #"{"doctype": "invoice"}"#)
}

// MARK: - decode

@Test("confidence as a fraction is kept")
func confidenceFraction() throws {
    let result = try MLXClassifier.decode(
        output: #"{"doctype": "invoice", "confidence": 0.9}"#,
        catalog: catalog,
        repo: "test"
    )
    #expect(result.doctype == "invoice")
    #expect(result.category == "financial")
    #expect(result.confidence == 0.9)
    #expect(result.engine == "mlx")
    #expect(result.contract == 1)
}

@Test("confidence on a 0-100 scale is rescaled")
func confidencePercent() throws {
    let result = try MLXClassifier.decode(
        output: #"{"doctype": "invoice", "confidence": 85}"#,
        catalog: catalog,
        repo: "test"
    )
    #expect(result.confidence == 0.85)
}

@Test("confidence as a string is parsed")
func confidenceString() throws {
    let result = try MLXClassifier.decode(
        output: #"{"doctype": "invoice", "confidence": "0.9"}"#,
        catalog: catalog,
        repo: "test"
    )
    #expect(result.confidence == 0.9)
}

@Test("a missing or unparsable confidence degrades to zero, never to a guess")
func confidenceMissing() throws {
    let missing = try MLXClassifier.decode(
        output: #"{"doctype": "invoice"}"#,
        catalog: catalog,
        repo: "test"
    )
    #expect(missing.confidence == 0)

    let junk = try MLXClassifier.decode(
        output: #"{"doctype": "invoice", "confidence": "very sure"}"#,
        catalog: catalog,
        repo: "test"
    )
    #expect(junk.confidence == 0)
}

@Test("a doctype outside the catalog is rejected, never invented")
func doctypeOutsideCatalog() {
    #expect(throws: HelperError.self) {
        try MLXClassifier.decode(
            output: #"{"doctype": "medical_record", "confidence": 0.99}"#,
            catalog: catalog,
            repo: "test"
        )
    }
}

@Test("doctype matching is case-insensitive and canonicalises to the catalog")
func doctypeCaseInsensitive() throws {
    let result = try MLXClassifier.decode(
        output: #"{"doctype": "Invoice", "confidence": 0.5}"#,
        catalog: catalog,
        repo: "test"
    )
    #expect(result.doctype == "invoice")
    #expect(result.category == "financial")
}

@Test("no JSON, malformed JSON and a missing doctype all fail cleanly")
func malformedAnswers() {
    #expect(throws: HelperError.self) {
        try MLXClassifier.decode(output: "I am not going to answer that.", catalog: catalog, repo: "test")
    }
    #expect(throws: HelperError.self) {
        try MLXClassifier.decode(output: #"{"doctype": }"#, catalog: catalog, repo: "test")
    }
    #expect(throws: HelperError.self) {
        try MLXClassifier.decode(output: #"{"confidence": 0.9}"#, catalog: catalog, repo: "test")
    }
}

@Test("fields are coerced to strings and blanks are dropped")
func fieldCoercion() throws {
    let result = try MLXClassifier.decode(
        output: """
            {"doctype": "invoice", "confidence": 0.9,
             "fields": {"amount": 4800, "rate": 12.5, "paid": false,
                        "issuer": "  ACME  ", "blank": "   ", "nested": {"a": 1}}}
            """,
        catalog: catalog,
        repo: "test"
    )
    #expect(result.fields["amount"] == "4800")
    #expect(result.fields["rate"] == "12.5")
    #expect(result.fields["paid"] == "false")
    #expect(result.fields["issuer"] == "ACME")
    #expect(result.fields["blank"] == nil)
    #expect(result.fields["nested"] == nil)
}

// MARK: - DocTypeCatalog

@Test("a spec entry with two colons is rejected rather than silently accepted")
func catalogRejectsExtraColon() {
    #expect(throws: HelperError.self) {
        try DocTypeCatalog(spec: "invoice:financial:extra")
    }
    #expect(throws: HelperError.self) {
        try DocTypeCatalog(spec: "invoice")
    }
    #expect(throws: HelperError.self) {
        try DocTypeCatalog(spec: "")
    }
    #expect(throws: HelperError.self) {
        try DocTypeCatalog(spec: ":financial")
    }
}

// MARK: - ModelCache

@Test("a repo id cannot escape the model cache")
func modelCacheRejectsTraversal() {
    for repo in ["../../etc", "mlx-community/../../../etc", "/etc/passwd", "", "a//b", "."] {
        #expect(throws: HelperError.self, "expected \(repo.debugDescription) to be rejected") {
            try ModelCache.directory(for: repo)
        }
    }
}

@Test("an ordinary repo id resolves inside the cache")
func modelCacheAcceptsRepoID() throws {
    let url = try ModelCache.directory(for: "mlx-community/Qwen2.5-3B-Instruct-4bit")
    #expect(url.path.hasPrefix(ModelCache.root.path))
    #expect(url.path.hasSuffix("kagaz/models/mlx-community/Qwen2.5-3B-Instruct-4bit"))
}
