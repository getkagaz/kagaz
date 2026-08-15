import CoreGraphics
import Foundation
import ImageIO
import Vision
import UniformTypeIdentifiers

/// On-device text recognition via Vision's `VNRecognizeTextRequest`.
///
/// Everything here is local: no model weights to fetch, no network. Images go
/// straight to Vision; PDFs are rasterised page by page with Core Graphics
/// first (PDFKit is avoided so the helper stays headless and AppKit-free).
enum VisionOCR {

    /// Default rasterisation density for PDF pages. 200 dpi is the sweet spot
    /// where Vision's accurate recogniser stops gaining accuracy on ordinary
    /// business documents.
    static let defaultDPI = 200.0

    /// Default page ceiling.
    ///
    /// This is a memory guard, not a policy: a rasterised page costs roughly
    /// 18 MB at 200 dpi, and the pipeline used to hold every page at once —
    /// a 49-page PDF peaked at 882 MB RSS and a 300-page scan would have
    /// reached several gigabytes before the OOM killer arrived. Pages are now
    /// streamed one at a time, so the ceiling exists only to keep a
    /// pathological input from running for an unbounded time. `--max-pages 0`
    /// still means "no limit" for callers that want it.
    static let defaultMaxPages = 200

    /// Runs recognition over the pages of `path`, one page at a time.
    ///
    /// - Parameters:
    ///   - path: image or PDF on disk.
    ///   - languages: BCP-47 tags, e.g. `["en-US", "hi-IN"]`. Empty means
    ///     Vision's own default ordering.
    ///   - dpi: PDF rasterisation density; ignored for images.
    ///   - maxPages: page ceiling; 0 means every page.
    static func run(path: String, languages: [String], dpi: Double, maxPages: Int) throws -> OCRResponse {
        let url = URL(fileURLWithPath: (path as NSString).expandingTildeInPath)
        guard FileManager.default.fileExists(atPath: url.path) else {
            throw HelperError(.fileNotFound, "no such file: \(url.path)")
        }
        guard FileManager.default.isReadableFile(atPath: url.path) else {
            throw HelperError(.fileNotFound, "file is not readable: \(url.path)")
        }
        guard maxPages >= 0 else {
            throw HelperError(.badUsage, "--max-pages cannot be negative")
        }

        let source = try PageSource(url: url, dpi: dpi)
        let total = source.pageCount
        guard total > 0 else {
            throw HelperError(.unsupportedFormat, "\(url.lastPathComponent) contains no pages")
        }
        let limit = maxPages > 0 ? min(maxPages, total) : total

        // The whole point of this loop: render one page, recognise it, drop the
        // bitmap, move on. Peak memory is one page, not the document. The
        // autorelease pool matters because ImageIO and Vision both hand back
        // autoreleased objects that would otherwise pile up until exit.
        var blocks: [OCRBlock] = []
        for number in 1...limit {
            try autoreleasepool {
                let image = try source.image(at: number)
                blocks.append(contentsOf: try recognise(image: image, page: number, languages: languages))
            }
        }

        guard !blocks.isEmpty else {
            throw HelperError(.noText, "no text recognised in \(url.lastPathComponent)")
        }

        return OCRResponse(
            contract: contractVersion,
            engine: "vision",
            confidence: overallConfidence(blocks),
            // A hit ceiling must be visible to the Go core, never silent: it is
            // the difference between "this document has no total" and "we never
            // looked at the page the total was on".
            pages: limit,
            totalPages: total,
            truncated: limit < total,
            blocks: blocks
        )
    }

    /// Length-weighted mean of block confidences: a long paragraph read well
    /// should not be dragged down by a doubtful two-character stamp.
    static func overallConfidence(_ blocks: [OCRBlock]) -> Double {
        var weighted = 0.0
        var total = 0.0
        for block in blocks {
            let weight = Double(max(block.text.count, 1))
            weighted += block.confidence * weight
            total += weight
        }
        guard total > 0 else { return 0 }
        return (weighted / total).rounded(toPlaces: 4)
    }

    // MARK: - Recognition

    private static func recognise(image: CGImage, page: Int, languages: [String]) throws -> [OCRBlock] {
        let request = VNRecognizeTextRequest()
        request.recognitionLevel = .accurate
        request.usesLanguageCorrection = true
        if !languages.isEmpty {
            request.recognitionLanguages = languages
        }

        let handler = VNImageRequestHandler(cgImage: image, options: [:])
        do {
            try handler.perform([request])
        } catch {
            throw HelperError(.ocrFailed, "Vision failed on page \(page): \(error.localizedDescription)")
        }

        guard let observations = request.results else { return [] }
        var blocks: [OCRBlock] = []
        for observation in observations {
            guard let candidate = observation.topCandidates(1).first else { continue }
            let text = candidate.string
            if text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { continue }
            blocks.append(
                OCRBlock(
                    text: text,
                    bbox: topLeftBox(observation.boundingBox),
                    confidence: Double(candidate.confidence).rounded(toPlaces: 4),
                    page: page
                )
            )
        }
        return blocks
    }

    /// Vision reports a normalised rect with the origin at the bottom-left.
    /// The contract is top-left, so y is flipped here and nowhere else.
    private static func topLeftBox(_ rect: CGRect) -> [Double] {
        [
            Double(rect.origin.x).rounded(toPlaces: 5),
            Double(1.0 - rect.origin.y - rect.size.height).rounded(toPlaces: 5),
            Double(rect.size.width).rounded(toPlaces: 5),
            Double(rect.size.height).rounded(toPlaces: 5),
        ]
    }

    // MARK: - Rasterisation

    /// A lazy page provider.
    ///
    /// It holds only the decoder — a `CGImageSource` or a `CGPDFDocument`, both
    /// of which keep the file mapped rather than decoded — and materialises one
    /// bitmap per `image(at:)` call. Nothing here ever accumulates pages; that
    /// is the whole reason this type exists instead of a `[CGImage]`.
    struct PageSource {
        private enum Backing {
            case image(CGImageSource)
            case pdf(CGPDFDocument)
        }

        private let backing: Backing
        private let name: String
        private let scale: Double

        /// Number of pages (PDF) or frames (image) available.
        let pageCount: Int

        init(url: URL, dpi: Double) throws {
            self.name = url.lastPathComponent
            self.scale = max(dpi, 36.0) / 72.0

            if VisionOCR.isPDF(url) {
                guard let document = CGPDFDocument(url as CFURL) else {
                    throw HelperError(.unsupportedFormat, "could not open \(name) as a PDF")
                }
                self.backing = .pdf(document)
                self.pageCount = document.numberOfPages
            } else {
                guard let source = CGImageSourceCreateWithURL(url as CFURL, nil) else {
                    throw HelperError(.unsupportedFormat, "could not open \(name) as an image or a PDF")
                }
                self.backing = .image(source)
                self.pageCount = CGImageSourceGetCount(source)
            }
        }

        /// Renders page `number` (1-based). The caller is expected to drop the
        /// result before asking for the next one.
        func image(at number: Int) throws -> CGImage {
            switch backing {
            case .image(let source):
                guard let image = CGImageSourceCreateImageAtIndex(source, number - 1, nil) else {
                    throw HelperError(.renderFailed, "could not decode frame \(number) of \(name)")
                }
                return image
            case .pdf(let document):
                guard let page = document.page(at: number) else {
                    throw HelperError(.renderFailed, "missing page \(number) in \(name)")
                }
                return try VisionOCR.render(page: page, number: number, scale: scale)
            }
        }
    }

    private static func isPDF(_ url: URL) -> Bool {
        if url.pathExtension.lowercased() == "pdf" { return true }
        guard let handle = try? FileHandle(forReadingFrom: url) else { return false }
        defer { try? handle.close() }
        let magic = try? handle.read(upToCount: 5)
        return magic == Data("%PDF-".utf8)
    }

    fileprivate static func render(page: CGPDFPage, number: Int, scale: Double) throws -> CGImage {
        let cropped = page.getBoxRect(.cropBox)
        let usesCropBox = !cropped.isEmpty
        let box = usesCropBox ? cropped : page.getBoxRect(.mediaBox)
        guard box.width > 0, box.height > 0 else {
            throw HelperError(.renderFailed, "page \(number) has an empty page box")
        }
        // A page carrying /Rotate 90 or 270 is stored landscape and displayed
        // portrait (or the reverse); the bitmap must match what a reader shows,
        // otherwise every line arrives sideways and Vision reads nothing.
        let quarterTurns = ((page.rotationAngle % 360) + 360) % 360
        let swapped = quarterTurns == 90 || quarterTurns == 270
        let boxWidth = Double(swapped ? box.height : box.width)
        let boxHeight = Double(swapped ? box.width : box.height)

        // Vision's own upper bound is generous, but a poster-sized page at
        // 200 dpi can still blow past what is useful; clamp the long edge.
        let maxPixels = 8000.0
        let effective = min(scale, maxPixels / max(boxWidth, boxHeight))
        let width = Int((boxWidth * effective).rounded())
        let height = Int((boxHeight * effective).rounded())
        guard width > 0, height > 0 else {
            throw HelperError(.renderFailed, "page \(number) rasterised to zero pixels")
        }

        guard let context = CGContext(
            data: nil,
            width: width,
            height: height,
            bitsPerComponent: 8,
            bytesPerRow: 0,
            space: CGColorSpaceCreateDeviceRGB(),
            bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue
        ) else {
            throw HelperError(.renderFailed, "could not allocate a \(width)x\(height) bitmap for page \(number)")
        }

        // PDFs frequently paint on transparency; Vision reads far better on a
        // white ground than on black.
        let target = CGRect(x: 0, y: 0, width: width, height: height)
        context.setFillColor(red: 1, green: 1, blue: 1, alpha: 1)
        context.fill(target)
        // getDrawingTransform folds the page box offset, the scale and the
        // page's own /Rotate into one matrix, so no manual flipping is needed.
        context.concatenate(
            page.getDrawingTransform(
                usesCropBox ? .cropBox : .mediaBox,
                rect: target,
                rotate: 0,
                preserveAspectRatio: true
            )
        )
        context.drawPDFPage(page)

        guard let image = context.makeImage() else {
            throw HelperError(.renderFailed, "could not rasterise page \(number)")
        }
        return image
    }
}

extension Double {
    /// Keeps the emitted JSON free of float noise like 0.9700000286102295.
    func rounded(toPlaces places: Int) -> Double {
        let factor = pow(10.0, Double(places))
        return (self * factor).rounded() / factor
    }
}
