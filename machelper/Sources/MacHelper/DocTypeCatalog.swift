import Foundation

/// The doctype catalog the Go core hands down on every `classify` call.
///
/// Wire format of `--doctypes` is a comma-separated list of `name:category`
/// pairs, e.g. `"invoice:financial,passport:identity,lease:property"`. The
/// helper never invents a doctype: guided generation is constrained to exactly
/// these names plus `unclassified`, and the category is looked up from this map
/// rather than being generated, so an out-of-catalog answer is impossible by
/// construction.
struct DocTypeCatalog {

    /// The one name the model may answer that is *not* a catalog entry: "none
    /// of these fits". It is not in `--doctypes` because it has no category —
    /// the Go core owns the same spelling as `doctypes.Unclassified`, and the
    /// two must stay identical or a decline would read as a hallucination.
    ///
    /// Without it the schema's `anyOf` makes declining structurally impossible,
    /// so the model returns its nearest miss at the same high confidence it
    /// would give a real match. A forced choice that is indistinguishable from
    /// a real one defeats constraint 8 in spirit even while satisfying it in
    /// letter, which is precisely what this member exists to prevent.
    static let unclassified = "unclassified"

    /// Doctype names in the order supplied, which is also the order shown to
    /// the model.
    let names: [String]
    private let categories: [String: String]

    /// Everything the model may answer: the catalog plus the escape hatch,
    /// which is appended last so the real doctypes are read first. A catalog
    /// that already contains "unclassified" is not given it twice — `anyOf`
    /// rejects duplicates.
    var choices: [String] {
        names.contains(Self.unclassified) ? names : names + [Self.unclassified]
    }

    /// Whether `doctype` is the escape hatch rather than a catalog entry.
    static func isUnclassified(_ doctype: String) -> Bool {
        doctype.lowercased() == unclassified
    }

    /// Parses the `--doctypes` value. Throws `.invalidDoctypes` when the spec
    /// is empty or an entry is not `name:category`.
    init(spec: String) throws {
        var names: [String] = []
        var categories: [String: String] = [:]
        for entry in spec.split(separator: ",") {
            let trimmed = entry.trimmingCharacters(in: .whitespaces)
            if trimmed.isEmpty { continue }
            // Exactly one colon. `maxSplits: 1` would happily turn "a:b:c" into
            // the category "b:c"; a contract parser that quietly accepts
            // malformed input is how a contract rots.
            let parts = trimmed.split(separator: ":", omittingEmptySubsequences: false)
            guard parts.count == 2 else {
                throw HelperError(.invalidDoctypes, "doctype entry \(trimmed.debugDescription) is not \"name:category\"")
            }
            let name = parts[0].trimmingCharacters(in: .whitespaces)
            let category = parts[1].trimmingCharacters(in: .whitespaces)
            guard !name.isEmpty, !category.isEmpty else {
                throw HelperError(.invalidDoctypes, "doctype entry \(trimmed.debugDescription) has an empty name or category")
            }
            if categories[name] == nil {
                names.append(name)
            }
            categories[name] = category
        }
        guard !names.isEmpty else {
            throw HelperError(.invalidDoctypes, "--doctypes is empty; expected \"name:category,...\"")
        }
        self.names = names
        self.categories = categories
    }

    /// The category for a doctype, or nil when the name is not in the catalog.
    func category(for doctype: String) -> String? {
        if let exact = categories[doctype] { return exact }
        let lowered = doctype.lowercased()
        for (name, category) in categories where name.lowercased() == lowered {
            return category
        }
        return nil
    }

    /// The catalog name matching `doctype` case-insensitively, so a model that
    /// answers "Invoice" still resolves to the catalog's "invoice".
    func canonical(_ doctype: String) -> String? {
        if categories[doctype] != nil { return doctype }
        let lowered = doctype.lowercased()
        return names.first { $0.lowercased() == lowered }
    }
}
