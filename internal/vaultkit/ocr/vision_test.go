package ocr

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

func TestParseVisionOutputReadingOrder(t *testing.T) {
	res, err := parseVisionOutput(readFixture(t, "vision_two_pages.json"))
	if err != nil {
		t.Fatalf("parseVisionOutput() error = %v", err)
	}

	// Page order first, then top-to-bottom within a page (Vision's y axis
	// points up, so the highest top edge is read first), then left-to-right.
	want := strings.Join([]string{
		"ACME LTD",
		"Invoice 2024-117",
		"Total due: 1,240.00",
		"Page 2 continues here",
	}, "\n")
	if res.Text != want {
		t.Fatalf("Text =\n%q\nwant\n%q", res.Text, want)
	}
	if res.Engine != "vision" {
		t.Fatalf("Engine = %q, want %q", res.Engine, "vision")
	}
	if res.Pages != 2 {
		t.Fatalf("Pages = %d, want 2", res.Pages)
	}
	if wantConf := (0.91 + 0.88 + 0.99 + 0.97) / 4; math.Abs(res.Confidence-wantConf) > 1e-9 {
		t.Fatalf("Confidence = %v, want %v", res.Confidence, wantConf)
	}
}

func TestParseVisionOutputContractVersions(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"future contract rejected", readFixture(t, "vision_future_contract.json"), "unsupported contract version 2"},
		{"missing contract rejected", []byte(`{"engine":"vision","blocks":[]}`), "unsupported contract version 0"},
		{"malformed json rejected", []byte(`{`), "decoding response"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseVisionOutput(tc.data)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseVisionOutputEmptyBlocks(t *testing.T) {
	res, err := parseVisionOutput([]byte(`{"contract":1,"engine":"vision","confidence":0,"blocks":[]}`))
	if err != nil {
		t.Fatalf("parseVisionOutput() error = %v", err)
	}
	if res.Text != "" || res.Pages != 0 {
		t.Fatalf("got %+v, want empty text and zero pages", res)
	}
}

func TestVisionUnavailableDegradesGracefully(t *testing.T) {
	withoutHelper(t)

	v := &Vision{Languages: []string{"en-US"}}
	if v.Available() {
		t.Fatal("Available() = true with no helper installed")
	}
	if d := v.detail(); !strings.Contains(d, "not found") {
		t.Fatalf("detail() = %q, want a not-found explanation", d)
	}
	if _, err := v.Extract(context.Background(), "scan.pdf"); !errors.Is(err, ErrNoHelper) {
		t.Fatalf("Extract() error = %v, want ErrNoHelper", err)
	}
}
