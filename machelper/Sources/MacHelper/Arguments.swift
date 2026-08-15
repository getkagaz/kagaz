import Foundation

/// A deliberately small hand-rolled argument parser.
///
/// Kagaz refuses a package dependency here (see Package.swift), so this stands
/// in for swift-argument-parser. It understands exactly what the contract needs:
///
///     --flag              boolean
///     --key value         option
///     --key=value         option
///     <positional>        anything not starting with "--"
///
/// A bare `--` ends option parsing; everything after it is positional.
struct Arguments {
    private(set) var positionals: [String] = []
    private var values: [String: String] = [:]
    private var flags: Set<String> = []

    /// Parses `argv` (excluding the executable path). `optionsTakingValue`
    /// lists the long names that consume the following argument when they are
    /// not written in `--key=value` form; anything else is a boolean flag.
    init(_ argv: [String], optionsTakingValue: Set<String>) throws {
        var index = 0
        var optionsEnded = false
        while index < argv.count {
            let arg = argv[index]
            index += 1

            if optionsEnded {
                positionals.append(arg)
                continue
            }
            if arg == "--" {
                optionsEnded = true
                continue
            }
            guard arg.hasPrefix("--") else {
                positionals.append(arg)
                continue
            }

            let body = String(arg.dropFirst(2))
            if body.isEmpty {
                throw HelperError(.badUsage, "empty option name")
            }
            if let eq = body.firstIndex(of: "=") {
                let name = String(body[body.startIndex..<eq])
                let value = String(body[body.index(after: eq)...])
                guard optionsTakingValue.contains(name) else {
                    throw HelperError(.badUsage, "option --\(name) does not take a value")
                }
                values[name] = value
                continue
            }
            if optionsTakingValue.contains(body) {
                guard index < argv.count else {
                    throw HelperError(.badUsage, "option --\(body) requires a value")
                }
                values[body] = argv[index]
                index += 1
                continue
            }
            flags.insert(body)
        }
    }

    /// True when the boolean flag was present.
    func flag(_ name: String) -> Bool {
        flags.contains(name)
    }

    /// The option's value, or nil when it was not supplied.
    func value(_ name: String) -> String? {
        values[name]
    }

    /// The option's value, or `fallback`.
    func value(_ name: String, default fallback: String) -> String {
        values[name] ?? fallback
    }

    /// The option's value parsed as an integer, or `fallback`.
    func intValue(_ name: String, default fallback: Int) throws -> Int {
        guard let raw = values[name] else { return fallback }
        guard let parsed = Int(raw) else {
            throw HelperError(.badUsage, "option --\(name) expects an integer, got \(raw.debugDescription)")
        }
        return parsed
    }

    /// Splits a comma-separated option value, trimming blanks and dropping
    /// empty entries.
    func listValue(_ name: String) -> [String] {
        guard let raw = values[name] else { return [] }
        return raw.split(separator: ",")
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty }
    }

    /// Every boolean flag seen, for rejecting typos.
    var seenFlags: Set<String> { flags }
}
