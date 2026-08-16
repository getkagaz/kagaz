package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/classify"
	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
	"github.com/spf13/cobra"
)

// Check statuses.
const (
	// CheckOK means the capability is present and working.
	CheckOK = "ok"
	// CheckWarn means an optional capability is missing: a feature degrades,
	// nothing breaks (Global Constraint 9).
	CheckWarn = "warn"
	// CheckFail means core function is broken.
	CheckFail = "fail"
)

// Check is one doctor result.
type Check struct {
	// Name is the stable check id, e.g. "pdftotext".
	Name string `json:"name"`
	// Status is CheckOK, CheckWarn or CheckFail.
	Status string `json:"status"`
	// Detail says what was found.
	Detail string `json:"detail"`
	// Impact says what stops working, for anything that is not ok.
	Impact string `json:"impact,omitempty"`
	// Fix is the suggested remedy, when there is one.
	Fix string `json:"fix,omitempty"`
}

// DoctorPayload is the `kagaz doctor --json` body.
type DoctorPayload struct {
	// OK is false when any check failed.
	OK bool `json:"ok"`
	// Checks are every check, in a stable order.
	Checks []Check `json:"checks"`
	// Counts is the number of checks per status.
	Counts map[string]int `json:"counts"`
	// Platform records what this build is running on, because half of these
	// capabilities only exist on macOS.
	Platform string `json:"platform"`
	// Version is the kagaz build version.
	Version string `json:"version"`
}

func newDoctorCommand(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the vault and the environment around it",
		Long: "doctor reports what Kagaz can and cannot do on this machine. A missing\n" +
			"optional tool is a warning and degrades one feature; only problems that break\n" +
			"core function set a non-zero exit.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			payload := runDoctor(rt)
			exit := ExitOK
			if !payload.OK {
				exit = ExitFailure
			}
			return rt.Emit(&Response{
				Command: "doctor", Status: StatusOK, Payload: payload, Human: humanDoctor, Exit: exit,
			})
		},
	}
}

// runDoctor performs every check. It never returns an error: reporting a
// broken environment is its entire job, and failing to run because the
// environment is broken would be a bad joke.
func runDoctor(rt *Runtime) DoctorPayload {
	payload := DoctorPayload{
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Version:  rt.Version,
		Counts:   map[string]int{CheckOK: 0, CheckWarn: 0, CheckFail: 0},
	}
	add := func(c Check) { payload.Checks = append(payload.Checks, c) }

	cfg, cfgErr := rt.Config()
	switch {
	case cfgErr != nil:
		add(Check{
			Name: "vault", Status: CheckFail, Detail: cfgErr.Error(),
			Impact: "every command that touches a vault will fail",
			Fix:    "run `kagaz init`, or pass --vault <path to vault.yaml>",
		})
	default:
		add(Check{Name: "vault", Status: CheckOK, Detail: cfg.Path})
		add(checkVaultRoot(cfg))
		add(checkXattr(cfg))
		add(checkCatalog(cfg))
	}

	add(checkTool("spotlight", "mdfind",
		"full-text queries fall back to a filesystem walk, which is slower but complete",
		"Spotlight ships with macOS; nothing to install"))
	add(checkTool("pdftotext", "pdftotext",
		"PDFs with a text layer need OCR instead of a millisecond text read",
		"brew install poppler"))
	add(checkHelper())
	add(checkTool("brctl", "brctl",
		"an iCloud-evicted document cannot be downloaded, and `kagaz resolve` will fail loudly on one",
		"brctl ships with macOS; nothing to install"))
	if cfgErr == nil {
		payload.Checks = append(payload.Checks, checkClassifiers(cfg)...)
		payload.Checks = append(payload.Checks, checkExtractors(cfg)...)
	}
	add(checkOllama(cfg, cfgErr))

	payload.OK = true
	for _, c := range payload.Checks {
		payload.Counts[c.Status]++
		if c.Status == CheckFail {
			payload.OK = false
		}
	}
	return payload
}

func checkVaultRoot(cfg *config.Config) Check {
	st, err := os.Stat(cfg.VaultRoot)
	switch {
	case err != nil:
		return Check{
			Name: "vault-root", Status: CheckFail, Detail: err.Error(),
			Impact: "there is nothing to search, lint or file into",
			Fix:    "create " + cfg.VaultRoot + ", or correct vault_root in vault.yaml",
		}
	case !st.IsDir():
		return Check{
			Name: "vault-root", Status: CheckFail, Detail: cfg.VaultRoot + " is not a directory",
			Impact: "there is nothing to search, lint or file into",
		}
	}
	return Check{Name: "vault-root", Status: CheckOK, Detail: cfg.VaultRoot}
}

// checkXattr writes and reads back a Finder tag on a temporary file inside the
// vault, because extended-attribute support is a property of the filesystem
// the vault happens to sit on, not of the operating system.
func checkXattr(cfg *config.Config) Check {
	probe, err := os.CreateTemp(cfg.VaultRoot, ".kagaz-doctor-*")
	if err != nil {
		return Check{
			Name: "xattr", Status: CheckWarn, Detail: "could not write a probe file: " + err.Error(),
			Impact: "Finder tags could not be tested",
		}
	}
	path := probe.Name()
	probe.Close()
	defer os.Remove(path)

	if err := tags.Write(path, []string{"kagaz-doctor"}); err != nil {
		return Check{
			Name: "xattr", Status: CheckWarn, Detail: err.Error(),
			Impact: "documents get no Finder tags, so mdfind and Finder smart folders will not find them by tag; " +
				"filenames, folders and sidecars still work",
			Fix: "keep the vault on an APFS/HFS+ volume",
		}
	}
	read, err := tags.Read(path)
	if err != nil || len(read) != 1 {
		return Check{
			Name: "xattr", Status: CheckWarn, Detail: "a tag was written but did not read back",
			Impact: "Finder tags may not survive on this filesystem",
		}
	}
	return Check{Name: "xattr", Status: CheckOK, Detail: "extended attributes work on this filesystem"}
}

func checkCatalog(cfg *config.Config) Check {
	cat, err := doctypes.Resolve(cfg)
	if err != nil {
		return Check{
			Name: "doctypes", Status: CheckFail, Detail: err.Error(),
			Impact: "classification and filing cannot run",
			Fix:    "correct the doctypes: block in vault.yaml",
		}
	}
	return Check{Name: "doctypes", Status: CheckOK, Detail: fmt.Sprintf("%d doctypes resolved", len(cat.Names()))}
}

// checkTool reports on an optional external binary.
func checkTool(name, binary, impact, fix string) Check {
	path, err := exec.LookPath(binary)
	if err != nil {
		return Check{Name: name, Status: CheckWarn, Detail: binary + " not found on PATH", Impact: impact, Fix: fix}
	}
	return Check{Name: name, Status: CheckOK, Detail: path}
}

func checkHelper() Check {
	path, ok := ocr.HelperPath()
	if !ok {
		return Check{
			Name: "kagaz-machelper", Status: CheckWarn,
			Detail: ocr.HelperBinary + " not found (looked in " + helperHint() + ")",
			Impact: "no Apple Vision OCR and no Apple Foundation Models classifier; " +
				"pdftotext and the offline rules classifier still work",
			Fix: "brew install kagaz (the formula builds and installs the helper)",
		}
	}
	return Check{Name: "kagaz-machelper", Status: CheckOK, Detail: path}
}

// checkClassifiers asks the classifier chain to describe itself, so doctor and
// the pipeline agree by construction about what is available.
func checkClassifiers(cfg *config.Config) []Check {
	chain := classify.New(cfg, nil)
	var out []Check
	for _, s := range chain.Describe() {
		c := Check{Name: "classify:" + s.Name, Status: CheckOK, Detail: s.Detail}
		if !s.Available {
			c.Status = CheckWarn
			if c.Detail == "" {
				c.Detail = "unavailable"
			}
			c.Impact = "this classifier tier is skipped; the chain falls back to the next one"
		}
		out = append(out, c)
	}
	if !chain.Available() {
		out = append(out, Check{
			Name: "classify", Status: CheckFail, Detail: "no classifier backend is usable",
			Impact: "ingest cannot classify anything",
			Fix:    "the offline rules classifier needs no installation; check classify.engine in vault.yaml",
		})
	}
	return out
}

// checkExtractors asks the OCR extractor to describe itself, for the same
// reason as checkClassifiers.
func checkExtractors(cfg *config.Config) []Check {
	var out []Check
	usable := false
	for _, s := range ocr.NewExtractor(cfg, "").Describe() {
		c := Check{Name: "ocr:" + s.Name, Status: CheckOK, Detail: s.Detail}
		if !s.Available {
			c.Status = CheckWarn
			if c.Detail == "" {
				c.Detail = "unavailable"
			}
			c.Impact = "this extraction tier is skipped"
		} else {
			usable = true
		}
		out = append(out, c)
	}
	if !usable {
		out = append(out, Check{
			Name: "ocr", Status: CheckWarn, Detail: "no text-extraction tier is available",
			Impact: "ingest can still file documents, but classification falls back to the filename alone",
			Fix:    "brew install poppler",
		})
	}
	return out
}

// checkOllama probes the local daemon. It is never a failure: Ollama is an
// opt-in tier, and the endpoint is required to be localhost either way.
func checkOllama(cfg *config.Config, cfgErr error) Check {
	endpoint := "http://localhost:11434"
	if cfgErr == nil && cfg.Classify.Endpoint != "" {
		endpoint = cfg.Classify.Endpoint
	}
	if _, err := exec.LookPath("ollama"); err != nil {
		return Check{
			Name: "ollama", Status: CheckWarn, Detail: "ollama not found on PATH",
			Impact: "the Ollama classifier and OCR tiers are unavailable",
			Fix:    "brew install ollama (optional)",
		}
	}
	return Check{Name: "ollama", Status: CheckOK, Detail: "installed; configured endpoint " + endpoint}
}

func humanDoctor(w io.Writer, payload any) error {
	p, ok := payload.(DoctorPayload)
	if !ok {
		return fmt.Errorf("doctor: unexpected payload %T", payload)
	}
	symbol := map[string]string{CheckOK: "ok  ", CheckWarn: "warn", CheckFail: "FAIL"}
	fmt.Fprintf(w, "kagaz %s on %s\n\n", p.Version, p.Platform)
	for _, c := range p.Checks {
		fmt.Fprintf(w, "%s  %-22s %s\n", symbol[c.Status], c.Name, c.Detail)
		if c.Impact != "" {
			fmt.Fprintf(w, "      %s\n", c.Impact)
		}
		if c.Fix != "" {
			fmt.Fprintf(w, "      fix: %s\n", c.Fix)
		}
	}
	fmt.Fprintf(w, "\n%d ok, %d warning(s), %d failure(s)\n",
		p.Counts[CheckOK], p.Counts[CheckWarn], p.Counts[CheckFail])
	if p.OK {
		fmt.Fprintln(w, "core function is available")
	}
	return nil
}

// helperHint names where a helper binary would be looked for, used in doctor
// output when it is missing.
func helperHint() string {
	exe, err := os.Executable()
	if err != nil {
		return strings.Join([]string{"$" + ocr.HelperPathEnv, "$PATH"}, ", ")
	}
	return strings.Join([]string{"$" + ocr.HelperPathEnv, filepath.Dir(exe), "$PATH"}, ", ")
}
