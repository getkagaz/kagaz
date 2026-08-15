// Package conventions implements the vault's naming grammar: how a document's
// facts become a filename, how a filename is read back into facts, and where in
// the folder tree the file belongs. Every rule here is driven by vault.yaml.
package conventions

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/fycal"
)

// Field names understood by the filename pattern.
const (
	FieldDocType    = "DocType"
	FieldNames      = "Names"
	FieldIdentifier = "Identifier"
	FieldYear       = "Year"
	FieldModifier   = "Modifier"
)

// Doc is the set of facts a filename encodes.
type Doc struct {
	DocType    string   // catalog doctype name, e.g. "invoice"
	Category   string   // catalog category, e.g. "financial"
	Owners     []string // person display names; empty means unowned/shared
	Identifier string   // issuer, counterparty or subject, e.g. "Acme Corp"
	Year       int      // 0 when absent
	Modifier   string   // free-form qualifier, e.g. "Final", "Renewal"
	Ext        string   // file extension including the dot, e.g. ".pdf"
}

// Conventions renders and parses names for one vault.
type Conventions struct {
	cfg      *config.Config
	segments []segment
}

type segment struct {
	field    string
	optional bool
}

var fieldRe = regexp.MustCompile(`\{([A-Za-z]+)\}`)

// New compiles the vault's filename pattern.
func New(cfg *config.Config) (*Conventions, error) {
	segs, err := parsePattern(cfg.Filename.Pattern)
	if err != nil {
		return nil, err
	}
	return &Conventions{cfg: cfg, segments: segs}, nil
}

// parsePattern turns "{A}_{B}[_{C}]" into an ordered field list. Literal text
// between fields is the field separator and carries no other meaning.
func parsePattern(pattern string) ([]segment, error) {
	var segs []segment
	optional := false
	seenRequiredAfterOptional := false
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '[':
			if optional {
				return nil, fmt.Errorf("filename.pattern: nested [ at offset %d", i)
			}
			optional = true
		case ']':
			if !optional {
				return nil, fmt.Errorf("filename.pattern: unmatched ] at offset %d", i)
			}
			optional = false
		case '{':
			end := strings.IndexByte(pattern[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("filename.pattern: unterminated { at offset %d", i)
			}
			name := pattern[i+1 : i+end]
			if !fieldRe.MatchString("{" + name + "}") {
				return nil, fmt.Errorf("filename.pattern: invalid field %q", name)
			}
			switch name {
			case FieldDocType, FieldNames, FieldIdentifier, FieldYear, FieldModifier:
			default:
				return nil, fmt.Errorf("filename.pattern: unknown field {%s}", name)
			}
			if !optional && len(segs) > 0 && segs[len(segs)-1].optional {
				seenRequiredAfterOptional = true
			}
			segs = append(segs, segment{field: name, optional: optional})
			i += end
		}
	}
	if optional {
		return nil, fmt.Errorf("filename.pattern: unmatched [")
	}
	if seenRequiredAfterOptional {
		return nil, fmt.Errorf("filename.pattern: required fields must precede optional ones")
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("filename.pattern: no fields")
	}
	return segs, nil
}

// Render produces the conventional base filename (including extension) for doc.
func (c *Conventions) Render(doc Doc) (string, error) {
	var parts []string
	for _, seg := range c.segments {
		v := c.fieldValue(doc, seg.field)
		if v == "" {
			if seg.optional {
				continue
			}
			return "", fmt.Errorf("filename: required field {%s} is empty", seg.field)
		}
		parts = append(parts, v)
	}
	name := strings.Join(parts, c.cfg.Filename.FieldSep)
	ext := doc.Ext
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return name + ext, nil
}

func (c *Conventions) fieldValue(doc Doc, field string) string {
	switch field {
	case FieldDocType:
		return c.Word(TitleCase(doc.DocType))
	case FieldNames:
		return c.RenderOwners(doc.Owners)
	case FieldIdentifier:
		return c.Word(doc.Identifier)
	case FieldYear:
		if doc.Year == 0 {
			return ""
		}
		return strconv.Itoa(doc.Year)
	case FieldModifier:
		return c.Word(doc.Modifier)
	default:
		return ""
	}
}

// RenderOwners joins owner display names for use inside a filename.
func (c *Conventions) RenderOwners(owners []string) string {
	if len(owners) == 0 {
		return ""
	}
	ordered := c.OrderOwners(owners)
	for i, o := range ordered {
		ordered[i] = c.Word(o)
	}
	return strings.Join(ordered, c.cfg.OwnerGroup.SeparatorFilename)
}

// OrderOwners applies the configured owner ordering, returning a copy.
func (c *Conventions) OrderOwners(owners []string) []string {
	out := append([]string(nil), owners...)
	if c.cfg.OwnerGroup.Order == "alphabetical" {
		sort.Slice(out, func(i, j int) bool {
			return strings.ToLower(out[i]) < strings.ToLower(out[j])
		})
	}
	return out
}

// Word normalises a free-text value into a single filename field: whitespace
// and stray separators collapse into the configured word separator.
func (c *Conventions) Word(s string) string {
	sep := c.cfg.Filename.WordSeparator
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '_', '/', '\\':
			return true
		}
		return strings.ContainsRune(c.cfg.Filename.FieldSep, r)
	})
	joined := strings.Join(fields, sep)
	// Collapse doubled separators introduced by the source text.
	for strings.Contains(joined, sep+sep) {
		joined = strings.ReplaceAll(joined, sep+sep, sep)
	}
	return strings.Trim(joined, sep)
}

// Parse reads a filename back into the facts it encodes. It is deliberately
// lenient: a name that does not match the grammar yields ok=false rather than
// an error, because lint needs to report such files, not fail on them.
func (c *Conventions) Parse(filename string) (Doc, bool) {
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filepath.Base(filename), ext)
	parts := strings.Split(base, c.cfg.Filename.FieldSep)

	var required, optional []segment
	for _, s := range c.segments {
		if s.optional {
			optional = append(optional, s)
		} else {
			required = append(required, s)
		}
	}
	if len(parts) < len(required) || len(parts) > len(c.segments) {
		return Doc{}, false
	}

	doc := Doc{Ext: ext}
	i := 0
	for _, seg := range required {
		c.assign(&doc, seg.field, parts[i])
		i++
	}
	// Fill trailing optionals in order. A Year slot only accepts something that
	// looks like a year, so "Invoice_Alex_Acme_Final" assigns Final to Modifier
	// rather than to Year.
	for _, seg := range optional {
		if i >= len(parts) {
			break
		}
		if seg.field == FieldYear && !looksLikeYear(parts[i]) {
			continue
		}
		c.assign(&doc, seg.field, parts[i])
		i++
	}
	if i != len(parts) {
		return Doc{}, false
	}
	if doc.DocType == "" {
		return Doc{}, false
	}
	return doc, true
}

func (c *Conventions) assign(doc *Doc, field, value string) {
	switch field {
	case FieldDocType:
		doc.DocType = config.Slug(value)
	case FieldNames:
		if value != "" {
			doc.Owners = strings.Split(value, c.cfg.OwnerGroup.SeparatorFilename)
			for i, o := range doc.Owners {
				doc.Owners[i] = strings.ReplaceAll(o, c.cfg.Filename.WordSeparator, " ")
			}
		}
	case FieldIdentifier:
		doc.Identifier = strings.ReplaceAll(value, c.cfg.Filename.WordSeparator, " ")
	case FieldYear:
		if y, err := strconv.Atoi(value); err == nil {
			doc.Year = y
		}
	case FieldModifier:
		doc.Modifier = strings.ReplaceAll(value, c.cfg.Filename.WordSeparator, " ")
	}
}

func looksLikeYear(s string) bool {
	if len(s) != 4 {
		return false
	}
	y, err := strconv.Atoi(s)
	return err == nil && y >= 1900 && y <= 2999
}

// Dir returns the absolute directory doc belongs in, expanding the category's
// layout template. {Owner} becomes the owner folder (or the category's shared
// folder for multi-owner documents) and {FY} the fiscal-year label.
func (c *Conventions) Dir(doc Doc) (string, error) {
	cat, ok := c.cfg.CategoryFor(doc.Category)
	if !ok {
		return "", fmt.Errorf("category %q is not defined in structure", doc.Category)
	}
	segs := []string{c.cfg.VaultRoot, cat.Path}
	for _, seg := range strings.Split(cat.Layout, "/") {
		switch seg {
		case "":
		case "{Owner}":
			segs = append(segs, c.OwnerFolder(doc.Owners, cat))
		case "{FY}":
			if doc.Year == 0 {
				continue
			}
			cal := fycal.New(c.cfg.FiscalYear.StartMonth, c.cfg.FiscalYear.LabelFormat)
			segs = append(segs, cal.YearStarting(doc.Year).Label())
		}
	}
	// Drop empty segments so an unowned document lands directly in the category.
	var kept []string
	for _, s := range segs {
		if s != "" {
			kept = append(kept, s)
		}
	}
	return filepath.Join(kept...), nil
}

// OwnerFolder is the folder name for a set of owners within cat.
func (c *Conventions) OwnerFolder(owners []string, cat config.Category) string {
	switch {
	case len(owners) == 0:
		if cat.Shared != "" {
			return cat.Shared
		}
		return ""
	case len(owners) == 1:
		return c.Word(owners[0])
	default:
		if cat.Shared != "" {
			return cat.Shared
		}
		ordered := c.OrderOwners(owners)
		for i, o := range ordered {
			ordered[i] = c.Word(o)
		}
		return strings.Join(ordered, c.cfg.OwnerGroup.SeparatorFolder)
	}
}

// Path is the full conventional absolute path for doc.
func (c *Conventions) Path(doc Doc) (string, error) {
	dir, err := c.Dir(doc)
	if err != nil {
		return "", err
	}
	name, err := c.Render(doc)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// TitleCase capitalises each dash-separated word: "boarding-pass" →
// "Boarding-Pass". Catalog doctypes are lowercase slugs; filenames are not.
func TitleCase(s string) string {
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		r := []rune(p)
		parts[i] = strings.ToUpper(string(r[0])) + string(r[1:])
	}
	return strings.Join(parts, "-")
}
