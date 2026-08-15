import Foundation

/// Where `kagaz model pull` puts MLX weights, and the only place this helper
/// will look for them.
///
/// Constraint 2 of the project is absolute: no network at inference time. The
/// helper therefore never touches HubApi's download path — it builds a
/// `ModelConfiguration(directory:)` pointing at an already-populated folder and
/// fails with `model_not_found` when that folder is missing or incomplete.
enum ModelCache {

    /// The default text model. It is a text LLM, not a VLM, which is why the
    /// classifier uses the MLXLLM text path rather than the VLM loader.
    static let defaultRepo = "mlx-community/Qwen2.5-3B-Instruct-4bit"

    /// `~/Library/Application Support/kagaz/models`
    static var root: URL {
        let home = FileManager.default.homeDirectoryForCurrentUser
        return home
            .appendingPathComponent("Library/Application Support/kagaz/models", isDirectory: true)
    }

    /// The directory for a Hugging Face repo id, e.g.
    /// `~/Library/Application Support/kagaz/models/mlx-community/Qwen2.5-3B-Instruct-4bit`.
    ///
    /// Throws on anything that would resolve outside the cache. Access here is
    /// read-only and the repo id comes from the user's own config, so the
    /// practical risk is small — but this binary is a security boundary, and a
    /// path builder that accepts `..` is not one worth defending later.
    static func directory(for repo: String) throws -> URL {
        let components = repo.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        guard !components.isEmpty, !repo.hasPrefix("/") else {
            throw HelperError(.badUsage, "--model must be a Hugging Face repo id like org/name, not a path")
        }
        var url = root
        for component in components {
            guard !component.isEmpty, component != ".", component != ".." else {
                throw HelperError(
                    .badUsage,
                    "--model \(repo.debugDescription) is not a valid repo id: components must not be empty, \".\" or \"..\""
                )
            }
            url.appendPathComponent(component, isDirectory: true)
        }
        return url
    }

    /// Resolves and validates the weight directory for `repo`.
    ///
    /// A directory counts as populated when it holds `config.json` and at least
    /// one `.safetensors` file. A half-finished pull is reported as
    /// `model_not_found` rather than being handed to MLX, because MLX's own
    /// error for that case is opaque.
    static func resolve(repo: String) throws -> URL {
        let directory = try directory(for: repo)
        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: directory.path, isDirectory: &isDirectory),
              isDirectory.boolValue else {
            throw HelperError(
                .modelNotFound,
                "no weights for \(repo) at \(directory.path); run: kagaz model pull \(repo)"
            )
        }
        let contents = (try? FileManager.default.contentsOfDirectory(atPath: directory.path)) ?? []
        guard contents.contains("config.json") else {
            throw HelperError(
                .modelNotFound,
                "\(directory.path) has no config.json; the pull looks incomplete — rerun: kagaz model pull \(repo)"
            )
        }
        guard contents.contains(where: { $0.hasSuffix(".safetensors") }) else {
            throw HelperError(
                .modelNotFound,
                "\(directory.path) has no .safetensors weights; the pull looks incomplete — rerun: kagaz model pull \(repo)"
            )
        }
        return directory
    }

    /// Non-throwing form used by `--probe`, which must stay fast and must not
    /// load anything.
    static func check(repo: String) -> ProbeResponse {
        do {
            _ = try resolve(repo: repo)
            return ProbeResponse(contract: contractVersion, engine: "mlx", available: true, reason: nil)
        } catch let error as HelperError {
            return ProbeResponse(contract: contractVersion, engine: "mlx", available: false, reason: error.message)
        } catch {
            return ProbeResponse(
                contract: contractVersion,
                engine: "mlx",
                available: false,
                reason: error.localizedDescription
            )
        }
    }
}
