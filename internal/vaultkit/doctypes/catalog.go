// Package doctypes holds the global document-type catalog: the built-in set of
// document kinds, their category, the high-precision keywords the offline
// fallback classifier matches on, and the regex templates used to extract
// structured fields.
//
// Nothing here is locale-specific. Country-specific documents are added
// per-vault through the `doctypes:` block in vault.yaml.
package doctypes

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

// DocType is one resolved entry of the catalog.
type DocType struct {
	Name     string
	Category string
	Keywords []string
	Patterns []*regexp.Regexp
	Extract  map[string]*regexp.Regexp

	keywordRes []*regexp.Regexp
}

// Catalog is the resolved doctype set for a vault: built-ins merged with any
// per-vault additions or overrides.
type Catalog struct {
	types  []*DocType
	byName map[string]*DocType
}

// builtin is the authoring form of a built-in doctype.
type builtin struct {
	name     string
	category string
	keywords []string
	patterns []string
	extract  map[string]string
}

// Shared extraction templates. Keeping them in one place means a fix to the
// date or amount regex lands everywhere at once.
const (
	reAmount = `(?i)(?:total|amount\s+due|grand\s+total|balance\s+due|net\s+payable|amount\s+payable)[^\d\-]{0,20}([0-9][0-9,]*(?:\.[0-9]{1,2})?)`
	reDate   = `(?i)(?:date(?:d)?|issued(?:\s+on)?|invoice\s+date)[:\s]{1,4}([0-9]{1,4}[-/. ][0-9]{1,2}[-/. ][0-9]{2,4}|[0-9]{1,2}\s+[A-Za-z]{3,9}\s+[0-9]{4})`
	reDueDate = `(?i)due\s+date[:\s]{1,4}([0-9]{1,4}[-/. ][0-9]{1,2}[-/. ][0-9]{2,4}|[0-9]{1,2}\s+[A-Za-z]{3,9}\s+[0-9]{4})`
)

// builtins is the global catalog. Keywords are matched on word boundaries and
// are chosen to be phrases a document of that kind almost always contains --
// generic words like "premium" or "platform" are deliberately excluded, because
// a keyword that fires on the wrong document is worse than no keyword at all.
var builtins = []builtin{
	// ---- financial -------------------------------------------------------
	{
		name: "invoice", category: "financial",
		keywords: []string{"invoice", "tax invoice", "invoice number", "invoice no", "bill to"},
		extract: map[string]string{
			"invoice_number": `(?i)invoice\s*(?:number|no\.?|#)[:\s]{0,4}([A-Z0-9][A-Z0-9\-/]{2,30})`,
			"amount":         reAmount,
			"date":           reDate,
			"due_date":       reDueDate,
		},
	},
	{
		name: "receipt", category: "financial",
		keywords: []string{"receipt", "payment received", "paid in full", "transaction receipt", "thank you for your payment"},
		extract: map[string]string{
			"receipt_number": `(?i)receipt\s*(?:number|no\.?|#)[:\s]{0,4}([A-Z0-9][A-Z0-9\-/]{2,30})`,
			"amount":         reAmount,
			"date":           reDate,
		},
	},
	{
		name: "bill", category: "utility",
		keywords: []string{"bill", "billing period", "amount due", "meter reading", "previous balance"},
		extract: map[string]string{
			"account_number": `(?i)account\s*(?:number|no\.?|#)[:\s]{0,4}([A-Z0-9][A-Z0-9\-/]{3,30})`,
			"amount":         reAmount,
			"due_date":       reDueDate,
		},
	},
	{
		name: "statement", category: "financial",
		keywords: []string{"account statement", "bank statement", "statement of account", "opening balance", "closing balance", "statement period"},
		extract: map[string]string{
			"account_number":  `(?i)account\s*(?:number|no\.?|#)[:\s]{0,4}([A-Z0-9X\*]{4,30})`,
			"closing_balance": `(?i)closing\s+balance[^\d\-]{0,20}([0-9][0-9,]*(?:\.[0-9]{1,2})?)`,
		},
	},
	{
		name: "payslip", category: "financial",
		keywords: []string{"payslip", "pay slip", "salary slip", "earnings statement", "net pay", "gross pay", "pay period"},
		extract: map[string]string{
			"net_pay":     `(?i)net\s+pay[^\d\-]{0,20}([0-9][0-9,]*(?:\.[0-9]{1,2})?)`,
			"gross_pay":   `(?i)gross\s+(?:pay|earnings)[^\d\-]{0,20}([0-9][0-9,]*(?:\.[0-9]{1,2})?)`,
			"pay_period":  `(?i)pay\s+period[:\s]{1,4}([A-Za-z0-9 ,\-/]{4,40})`,
			"employee_id": `(?i)employee\s*(?:id|number|no\.?|code)[:\s]{0,4}([A-Z0-9\-]{2,20})`,
		},
	},
	{
		name: "tax-return", category: "financial",
		keywords: []string{"tax return", "income tax return", "taxable income", "assessment year", "tax year", "self assessment"},
		extract: map[string]string{
			"tax_year":       `(?i)(?:assessment|tax)\s+year[:\s]{1,4}([0-9]{4}(?:\s*[-/]\s*[0-9]{2,4})?)`,
			"taxable_income": `(?i)taxable\s+income[^\d\-]{0,20}([0-9][0-9,]*(?:\.[0-9]{1,2})?)`,
		},
	},
	{
		name: "tax-certificate", category: "financial",
		keywords: []string{"tax deducted at source", "withholding tax certificate", "tax certificate", "certificate of tax"},
	},

	// ---- identity --------------------------------------------------------
	{
		name: "passport", category: "identity",
		keywords: []string{"passport", "passport no", "passport number", "type p", "date of expiry", "place of issue"},
		patterns: []string{`P<[A-Z]{3}[A-Z<]{5,}`}, // machine-readable zone
		extract: map[string]string{
			"passport_number": `(?i)passport\s*(?:number|no\.?)[:\s]{0,4}([A-Z0-9]{6,12})`,
			"expiry":          `(?i)date\s+of\s+expiry[:\s]{1,4}([0-9]{1,2}[ /.\-][A-Za-z0-9]{2,9}[ /.\-][0-9]{2,4})`,
			"nationality":     `(?i)nationality[:\s]{1,4}([A-Z][A-Za-z ]{3,30})`,
		},
	},
	{
		name: "visa", category: "identity",
		keywords: []string{"visa", "visa number", "entries", "duration of stay", "visa type"},
		extract: map[string]string{
			"visa_number": `(?i)visa\s*(?:number|no\.?)[:\s]{0,4}([A-Z0-9]{6,20})`,
			"expiry":      `(?i)(?:valid\s+until|expiry|expires)[:\s]{1,4}([0-9]{1,2}[ /.\-][A-Za-z0-9]{2,9}[ /.\-][0-9]{2,4})`,
		},
	},
	{
		name: "national-id", category: "identity",
		keywords: []string{"national identity card", "national id", "identity card", "citizen card"},
	},
	{
		name: "drivers-license", category: "identity",
		keywords: []string{"driving licence", "driver's license", "drivers license", "driving license", "licence to drive"},
		extract: map[string]string{
			"licence_number": `(?i)(?:licence|license)\s*(?:number|no\.?)[:\s]{0,4}([A-Z0-9\-]{5,25})`,
			"expiry":         `(?i)(?:valid\s+until|expiry|expires)[:\s]{1,4}([0-9]{1,2}[ /.\-][A-Za-z0-9]{2,9}[ /.\-][0-9]{2,4})`,
		},
	},
	{
		name: "birth-certificate", category: "identity",
		keywords: []string{"birth certificate", "certificate of birth", "date of birth registered"},
	},
	{
		name: "marriage-certificate", category: "legal",
		keywords: []string{"marriage certificate", "certificate of marriage", "solemnized"},
	},

	// ---- travel ----------------------------------------------------------
	{
		name: "itinerary", category: "travel",
		keywords: []string{"itinerary", "travel itinerary", "booking reference", "departure", "arrival", "trip summary"},
		extract: map[string]string{
			"booking_reference": `(?i)(?:booking\s+reference|pnr|confirmation\s+(?:number|code))[:\s]{0,4}([A-Z0-9]{5,10})`,
		},
	},
	{
		name: "ticket", category: "travel",
		keywords: []string{"e-ticket", "eticket", "ticket number", "passenger name", "fare basis"},
		extract: map[string]string{
			"ticket_number": `(?i)ticket\s*(?:number|no\.?)[:\s]{0,4}([0-9\-]{9,20})`,
		},
	},
	{
		name: "boarding-pass", category: "travel",
		keywords: []string{"boarding pass", "boarding time", "seat", "gate", "group boarding"},
		extract: map[string]string{
			"flight": `(?i)flight[:\s]{1,4}([A-Z0-9]{2}\s?[0-9]{1,4})`,
			"seat":   `(?i)seat[:\s]{1,4}([0-9]{1,2}[A-Z])`,
		},
	},
	{
		name: "hotel-booking", category: "travel",
		keywords: []string{"hotel booking", "reservation confirmation", "check-in", "check-out", "room type"},
	},

	// ---- insurance -------------------------------------------------------
	{
		name: "insurance-policy", category: "insurance",
		keywords: []string{"insurance policy", "policy schedule", "policy number", "sum insured", "policy holder", "coverage period"},
		extract: map[string]string{
			"policy_number": `(?i)policy\s*(?:number|no\.?)[:\s]{0,4}([A-Z0-9][A-Z0-9\-/]{4,30})`,
			"sum_insured":   `(?i)sum\s+insured[^\d\-]{0,20}([0-9][0-9,]*(?:\.[0-9]{1,2})?)`,
			"expiry":        `(?i)(?:valid\s+until|expiry|expires|period\s+of\s+insurance\s+to)[:\s]{1,4}([0-9]{1,2}[ /.\-][A-Za-z0-9]{2,9}[ /.\-][0-9]{2,4})`,
		},
	},
	{
		name: "insurance-claim", category: "insurance",
		keywords: []string{"insurance claim", "claim number", "claim form", "claim settlement"},
		extract: map[string]string{
			"claim_number": `(?i)claim\s*(?:number|no\.?)[:\s]{0,4}([A-Z0-9][A-Z0-9\-/]{4,30})`,
		},
	},

	// ---- medical ---------------------------------------------------------
	{
		name: "prescription", category: "medical",
		keywords: []string{"prescription", "rx", "dosage", "prescribed by", "refills"},
	},
	{
		name: "lab-report", category: "medical",
		keywords: []string{"lab report", "laboratory report", "test report", "reference range", "specimen", "pathology"},
	},
	{
		name: "medical-record", category: "medical",
		keywords: []string{"discharge summary", "medical record", "diagnosis", "consultation note", "treatment plan"},
	},
	{
		name: "vaccination-certificate", category: "medical",
		keywords: []string{"vaccination certificate", "immunisation record", "immunization record", "dose administered"},
	},

	// ---- legal / property / vehicles -------------------------------------
	{
		name: "contract", category: "legal",
		keywords: []string{"this agreement", "agreement is made", "terms and conditions", "hereinafter referred to as", "in witness whereof"},
		extract: map[string]string{
			"effective_date": `(?i)effective\s+date[:\s]{1,4}([0-9]{1,2}[ /.\-][A-Za-z0-9]{2,9}[ /.\-][0-9]{2,4})`,
		},
	},
	{
		name: "nda", category: "legal",
		keywords: []string{"non-disclosure agreement", "confidentiality agreement", "confidential information means"},
	},
	{
		name: "power-of-attorney", category: "legal",
		keywords: []string{"power of attorney", "attorney-in-fact", "hereby appoint"},
	},
	{
		name: "court-document", category: "legal",
		keywords: []string{"in the court of", "case number", "petitioner", "respondent", "hereby ordered"},
	},
	{
		name: "lease-agreement", category: "property",
		keywords: []string{"lease agreement", "rental agreement", "tenancy agreement", "lessor", "lessee", "monthly rent"},
		extract: map[string]string{
			"rent": `(?i)(?:monthly\s+)?rent[^\d\-]{0,20}([0-9][0-9,]*(?:\.[0-9]{1,2})?)`,
		},
	},
	{
		name: "property-deed", category: "property",
		keywords: []string{"sale deed", "title deed", "conveyance deed", "property registration", "schedule of property"},
	},
	{
		name: "property-tax", category: "property",
		keywords: []string{"property tax", "municipal tax", "assessment number", "holding tax"},
	},
	{
		name: "vehicle-registration", category: "vehicles",
		keywords: []string{"vehicle registration", "registration certificate", "chassis number", "engine number", "vehicle identification number"},
		extract: map[string]string{
			"registration_number": `(?i)registration\s*(?:number|no\.?|mark)[:\s]{0,4}([A-Z0-9\- ]{5,15})`,
			"vin":                 `(?i)(?:vin|chassis\s*(?:number|no\.?))[:\s]{0,4}([A-HJ-NPR-Z0-9]{11,17})`,
		},
	},
	{
		name: "vehicle-service", category: "vehicles",
		keywords: []string{"service record", "vehicle service", "odometer", "next service due"},
	},

	// ---- company ---------------------------------------------------------
	{
		name: "incorporation-certificate", category: "company",
		keywords: []string{"certificate of incorporation", "incorporated under", "company registration number", "certificate of formation"},
	},
	{
		name: "purchase-order", category: "company",
		keywords: []string{"purchase order", "po number", "ship to", "ordered by"},
		extract: map[string]string{
			"po_number": `(?i)(?:purchase\s+order|p\.?o\.?)\s*(?:number|no\.?|#)[:\s]{0,4}([A-Z0-9][A-Z0-9\-/]{2,30})`,
			"amount":    reAmount,
		},
	},
	{
		name: "quotation", category: "company",
		keywords: []string{"quotation", "quote number", "proposal valid until", "estimate for"},
	},
	{
		name: "annual-report", category: "company",
		keywords: []string{"annual report", "directors' report", "auditor's report", "balance sheet as at"},
	},

	// ---- personal --------------------------------------------------------
	{
		name: "certificate", category: "personal",
		keywords: []string{"certificate of completion", "hereby certifies", "certificate of achievement"},
	},
	{
		name: "transcript", category: "personal",
		keywords: []string{"academic transcript", "transcript of records", "grade point average", "marks obtained"},
	},
	{
		name: "resume", category: "personal",
		keywords: []string{"curriculum vitae", "work experience", "professional summary"},
	},
	{
		name: "correspondence", category: "personal",
		keywords: []string{"dear sir", "dear madam", "yours sincerely", "yours faithfully"},
	},
}

// Unclassified is the doctype assigned when nothing matches with confidence.
// It is a real doctype so that ingest always has something to propose and lint
// can report it, but it is never inferred from a category.
const Unclassified = "unclassified"

// Resolve builds the catalog for a vault: built-ins plus vault.yaml additions,
// where an entry with a built-in name replaces that built-in entirely.
func Resolve(cfg *config.Config) (*Catalog, error) {
	cat := &Catalog{byName: map[string]*DocType{}}

	for _, b := range builtins {
		dt, err := compile(b.name, b.category, b.keywords, b.patterns, b.extract)
		if err != nil {
			return nil, fmt.Errorf("builtin doctype %s: %w", b.name, err)
		}
		// A built-in whose category is not in this vault's structure is simply
		// unavailable here; a trimmed-down vault stays valid.
		if _, ok := cfg.Structure[dt.Category]; !ok {
			continue
		}
		cat.add(dt)
	}

	for _, d := range cfg.DocTypes {
		name := config.Slug(d.Name)
		dt, err := compile(name, d.Category, d.Match.Keywords, d.Match.Patterns, d.Extract)
		if err != nil {
			return nil, fmt.Errorf("doctypes.%s: %w", d.Name, err)
		}
		cat.replace(dt)
	}

	sort.Slice(cat.types, func(i, j int) bool { return cat.types[i].Name < cat.types[j].Name })
	return cat, nil
}

func compile(name, category string, keywords, patterns []string, extract map[string]string) (*DocType, error) {
	dt := &DocType{
		Name:     name,
		Category: category,
		Keywords: keywords,
		Extract:  map[string]*regexp.Regexp{},
	}
	for _, kw := range keywords {
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(strings.ToLower(kw)) + `\b`)
		if err != nil {
			return nil, fmt.Errorf("keyword %q: %w", kw, err)
		}
		dt.keywordRes = append(dt.keywordRes, re)
	}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		dt.Patterns = append(dt.Patterns, re)
	}
	for field, p := range extract {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("extract.%s %q: %w", field, p, err)
		}
		if re.NumSubexp() < 1 {
			return nil, fmt.Errorf("extract.%s %q: needs a capture group", field, p)
		}
		dt.Extract[field] = re
	}
	return dt, nil
}

func (c *Catalog) add(dt *DocType) {
	if _, exists := c.byName[dt.Name]; exists {
		return
	}
	c.types = append(c.types, dt)
	c.byName[dt.Name] = dt
}

func (c *Catalog) replace(dt *DocType) {
	if old, exists := c.byName[dt.Name]; exists {
		for i, t := range c.types {
			if t == old {
				c.types[i] = dt
			}
		}
		c.byName[dt.Name] = dt
		return
	}
	c.add(dt)
}

// Get returns the named doctype.
func (c *Catalog) Get(name string) (*DocType, bool) {
	dt, ok := c.byName[config.Slug(name)]
	return dt, ok
}

// Has reports whether name is in the catalog.
func (c *Catalog) Has(name string) bool {
	_, ok := c.byName[config.Slug(name)]
	return ok
}

// All returns every doctype, name-sorted.
func (c *Catalog) All() []*DocType { return c.types }

// Names returns every doctype name, sorted.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.types))
	for _, t := range c.types {
		out = append(out, t.Name)
	}
	return out
}

// Spec renders the catalog as the compact "name:category,…" string passed to
// the Swift helper so the model's output is constrained to real doctypes.
func (c *Catalog) Spec() string {
	parts := make([]string, 0, len(c.types))
	for _, t := range c.types {
		parts = append(parts, t.Name+":"+t.Category)
	}
	return strings.Join(parts, ",")
}

// CategoryOf returns the category for a doctype name.
func (c *Catalog) CategoryOf(name string) (string, bool) {
	dt, ok := c.Get(name)
	if !ok {
		return "", false
	}
	return dt.Category, true
}
