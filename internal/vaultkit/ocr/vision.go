package ocr

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// VisionContract is the JSON contract version this package understands. The
// helper stamps every response with "contract"; a mismatch is rejected rather
// than guessed at, so an upgraded helper never feeds a stale Go core garbage.
const VisionContract = 1

// Vision runs Apple's Vision framework text recogniser through
// kagaz-machelper. It needs no model weights and costs roughly a second per
// page, which makes it the default runner for scans.
type Vision struct {
	// Languages are BCP-47 recognition languages, e.g. "en-US". Empty means
	// the helper's own default.
	Languages []string
}

// Name identifies the runner in Result.Engine and doctor output.
func (v *Vision) Name() string { return "vision" }

// Available reports whether kagaz-machelper can be found.
func (v *Vision) Available() bool {
	_, ok := HelperPath()
	return ok
}

// detail explains, for `kagaz doctor`, either where the helper was found or why
// the runner is unusable.
func (v *Vision) detail() string {
	if path, ok := HelperPath(); ok {
		if len(v.Languages) > 0 {
			return path + " (langs " + strings.Join(v.Languages, ",") + ")"
		}
		return path
	}
	return HelperBinary + " not found (macOS only; set $" + HelperPathEnv + " for a local build)"
}

// Extract runs `kagaz-machelper ocr <path> --langs a,b --json` and decodes the
// versioned block contract into a single reading-order string.
func (v *Vision) Extract(ctx context.Context, path string) (Result, error) {
	args := []string{"ocr", path}
	if len(v.Languages) > 0 {
		args = append(args, "--langs", strings.Join(v.Languages, ","))
	}
	args = append(args, "--json")

	out, err := RunHelper(ctx, args...)
	if err != nil {
		return Result{Engine: "none"}, err
	}
	return parseVisionOutput(out)
}

// visionResponse mirrors §4.4 of the machelper contract.
type visionResponse struct {
	Contract   int           `json:"contract"`
	Engine     string        `json:"engine"`
	Confidence float64       `json:"confidence"`
	Blocks     []visionBlock `json:"blocks"`
}

// visionBlock is one recognised text region.
//
// BBox is [x, y, width, height] in normalised (0-1) page coordinates with the
// origin at the **top left**, per docs/machelper-contract.md. Vision's native
// bottom-left rectangle is flipped inside the helper
// (MacHelper/VisionOCR.swift topLeftBox), so a smaller y is higher up the page
// and reading order is ascending y.
type visionBlock struct {
	Text       string    `json:"text"`
	BBox       []float64 `json:"bbox"`
	Confidence float64   `json:"confidence"`
	Page       int       `json:"page"`
}

// top returns the block's top edge, which in top-left coordinates is simply y.
// A malformed or absent bbox sorts to the top of the page.
func (b visionBlock) top() float64 {
	if len(b.BBox) >= 2 {
		return b.BBox[1]
	}
	return 0
}

// left returns the block's left edge, or 0 for a malformed bbox.
func (b visionBlock) left() float64 {
	if len(b.BBox) >= 1 {
		return b.BBox[0]
	}
	return 0
}

// parseVisionOutput decodes the helper's JSON, orders blocks for reading, and
// summarises confidence and page count.
func parseVisionOutput(data []byte) (Result, error) {
	var resp visionResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return Result{Engine: "none"}, fmt.Errorf("%s ocr: decoding response: %w", HelperBinary, err)
	}
	if resp.Contract != VisionContract {
		return Result{Engine: "none"}, fmt.Errorf(
			"%s ocr: unsupported contract version %d (this build understands %d); upgrade kagaz or the helper so they match",
			HelperBinary, resp.Contract, VisionContract)
	}

	blocks := make([]visionBlock, len(resp.Blocks))
	copy(blocks, resp.Blocks)
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].Page != blocks[j].Page {
			return blocks[i].Page < blocks[j].Page
		}
		if ti, tj := blocks[i].top(), blocks[j].top(); ti != tj {
			return ti < tj // smaller y is higher on the page, so it is read first
		}
		return blocks[i].left() < blocks[j].left()
	})

	var (
		lines    []string
		confSum  float64
		confN    int
		maxPage  int
		anyBlock = len(blocks) > 0
	)
	for _, b := range blocks {
		if b.Page > maxPage {
			maxPage = b.Page
		}
		if t := strings.TrimSpace(b.Text); t != "" {
			lines = append(lines, t)
		}
		confSum += b.Confidence
		confN++
	}

	confidence := resp.Confidence
	if confN > 0 {
		confidence = confSum / float64(confN)
	}
	pages := maxPage
	if pages < 1 && anyBlock {
		pages = 1
	}

	return Result{
		Text:       strings.Join(lines, "\n"),
		Engine:     "vision",
		Confidence: confidence,
		Pages:      pages,
	}, nil
}
