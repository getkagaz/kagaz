import Darwin
import Foundation
import Metal

/// Whether MLX can actually run on this machine, checked without loading a model.
///
/// This exists because a `kagaz-machelper-mlx` binary can link and start
/// perfectly while being incapable of executing a single MLX operation. SwiftPM
/// has no build rule for `.metal` sources — mlx-swift's own README says the
/// shaders "have to be" built by Xcode — so `swift build` happily produces a
/// binary with no compiled shader library next to it. The first MLX op then
/// dies with `Failed to load the default metallib`, from C++, mid-run.
///
/// `--probe` is what the Go `Chain` consults before it selects this tier, so a
/// probe that reports "the binary exists" instead of "inference can run" makes
/// the Chain pick a tier that fails on every document. Everything here is
/// therefore a *runtime* check, and cheap enough to run on each classification:
/// create the Metal device, find the shader library the way mlx finds it, and
/// load it. That is single-digit milliseconds, versus tens of seconds to load
/// a 3B model.
enum MetalRuntime {

    /// File name mlx's own CMake build gives the pre-compiled shader library,
    /// and the name it looks for beside the running binary.
    static let colocatedLibraryName = "mlx.metallib"

    /// Resource bundle mlx falls back to, from `SWIFTPM_BUNDLE` in mlx-swift's
    /// `Package.swift`. Only an Xcode build ever populates it.
    static let bundleName = "mlx-swift_Cmlx"

    /// The shader library mlx would load, or `nil` if it would find none.
    ///
    /// Mirrors the search order in mlx `backend/metal/device.cpp`
    /// (`load_default_library`): colocated `mlx.metallib`, colocated
    /// `Resources/mlx.metallib`, then `default.metallib` inside a
    /// `mlx-swift_Cmlx.bundle`. Deliberately *not* broader than mlx's own list
    /// — a probe that finds the file somewhere mlx will not look is a probe
    /// that lies in the other direction.
    static func shaderLibraryURL() -> URL? {
        var candidates: [URL] = []
        for directory in binaryDirectories() {
            candidates.append(directory.appendingPathComponent(colocatedLibraryName))
            candidates.append(
                directory
                    .appendingPathComponent("Resources", isDirectory: true)
                    .appendingPathComponent(colocatedLibraryName)
            )
        }
        for root in bundleSearchRoots() {
            let bundle = root.appendingPathComponent("\(bundleName).bundle", isDirectory: true)
            candidates.append(
                bundle
                    .appendingPathComponent("Contents/Resources", isDirectory: true)
                    .appendingPathComponent("default.metallib")
            )
            candidates.append(bundle.appendingPathComponent("default.metallib"))
        }
        return candidates.first { FileManager.default.fileExists(atPath: $0.path) }
    }

    /// `nil` when MLX inference can run; otherwise the reason it cannot, phrased
    /// for a human reading `kagaz doctor` output.
    ///
    /// The library is not merely located, it is handed to the Metal device. A
    /// metallib built for the wrong target, truncated by a failed install or
    /// unreadable is exactly the situation that made the old probe lie, and
    /// only an actual load rules it out.
    static func unavailableReason() -> String? {
        unavailable()?.reason
    }

    /// The prose reason and its machine-readable code, or `nil` when MLX can
    /// run. The code exists so a client can tell "no shader library" (rebuild)
    /// from "no weights" (download) without reading the sentence.
    static func unavailable() -> (reason: String, code: ProbeReasonCode)? {
        guard let device = MTLCreateSystemDefaultDevice() else {
            return ("no Metal device is available; MLX needs an Apple silicon GPU", .noMetalDevice)
        }
        guard let url = shaderLibraryURL() else {
            let location = binaryDirectories().first?.path ?? "the directory holding this binary"
            return ("""
                MLX shader library not found: no \(colocatedLibraryName) beside the helper at \
                \(location), and no \(bundleName).bundle. SwiftPM cannot compile MLX's Metal \
                kernels, so `swift build` alone does not produce it; build it with \
                Scripts/build-metallib.sh and install it next to kagaz-machelper-mlx
                """, .shaderLibraryMissing)
        }
        do {
            _ = try device.makeLibrary(URL: url)
        } catch {
            return (
                "MLX shader library at \(url.path) could not be loaded: \(error.localizedDescription)",
                .shaderLibraryMissing
            )
        }
        return nil
    }

    /// Throwing form used on the classify path, so a broken install is a
    /// structured `backend_unavailable` rather than an MLX C++ abort.
    static func requireAvailable() throws {
        if let reason = unavailableReason() {
            throw HelperError(.backendUnavailable, reason)
        }
    }

    // MARK: - Locations

    /// Directories that count as "beside the running binary", most authoritative
    /// first.
    ///
    /// mlx's `current_binary_dir()` is `dladdr` on one of its own symbols, and
    /// `dli_fname` for the main image is the **symlink-resolved** path — verified
    /// on this platform, not assumed. A Homebrew install invoked through
    /// `/opt/homebrew/bin/kagaz-machelper-mlx` therefore has mlx looking inside
    /// the Cellar, so that is the first candidate. The unresolved launch
    /// directory is kept as a second candidate: it costs nothing and covers a
    /// layout where the library sits beside the symlink instead.
    static func binaryDirectories() -> [URL] {
        guard let executable = executableURL() else { return [] }
        let resolved = executable.resolvingSymlinksInPath().deletingLastPathComponent()
        let launched = executable.deletingLastPathComponent()
        var seen = Set<String>()
        return [resolved, launched].filter { seen.insert($0.standardizedFileURL.path).inserted }
    }

    /// Path of the running executable exactly as it was launched.
    static func executableURL() -> URL? {
        var size = UInt32(0)
        _ = _NSGetExecutablePath(nil, &size)
        guard size > 0 else { return nil }
        var buffer = [CChar](repeating: 0, count: Int(size))
        guard _NSGetExecutablePath(&buffer, &size) == 0 else { return nil }
        let path = String(cString: buffer)
        guard !path.isEmpty else { return nil }
        return URL(fileURLWithPath: path)
    }

    private static func bundleSearchRoots() -> [URL] {
        var roots: [URL] = [Bundle.main.bundleURL]
        if let resources = Bundle.main.resourceURL {
            roots.append(resources)
        }
        for bundle in Bundle.allBundles {
            if let resources = bundle.resourceURL {
                roots.append(resources)
            }
        }
        var seen = Set<String>()
        return roots.filter { seen.insert($0.standardizedFileURL.path).inserted }
    }
}
