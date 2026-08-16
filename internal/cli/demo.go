package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/conventions"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
	"github.com/getkagaz/kagaz/internal/vaultkit/fycal"
	"github.com/getkagaz/kagaz/internal/vaultkit/move"
	"github.com/getkagaz/kagaz/internal/vaultkit/sidecar"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// demoPerson is one seeded vault owner.
type demoPerson struct {
	Name string
	Tag  string
}

// demoPeople are the two people the demo vault files documents for. They are
// obviously fictional on purpose: a demo vault must never look like it was
// assembled from somebody's real paperwork.
var demoPeople = []demoPerson{
	{Name: "Alex Rao", Tag: "alex-rao"},
	{Name: "Sam Rao", Tag: "sam-rao"},
}

// demoDoc describes one synthetic document. Everything the demo vault contains
// — the files, the sidecars, the Finder tags and the tag vocabulary written
// into vault.yaml — is derived from this one table, so the vocabulary can never
// drift out of step with the documents and leave `kagaz lint` complaining about
// a vault Kagaz itself just created.
type demoDoc struct {
	DocType    string
	Category   string
	Owners     []string
	Identifier string
	Year       int
	// Company is the company tag, empty when the counterparty is not one.
	Company string
	// Areas are area tags.
	Areas []string
	// Lifecycle is the lifecycle tag, always one of the built-in set.
	Lifecycle string
	// Extra are further lifecycle-vocabulary tags, e.g. "confidential".
	Extra []string
	// Body is the document text, also stored in the sidecar.
	Body []string
}

var demoDocs = []demoDoc{
	{
		DocType: "invoice", Category: "financial", Owners: []string{"Alex Rao"},
		Identifier: "Acme Corp", Year: 2026, Company: "acme-corp", Lifecycle: "active",
		Body: []string{
			"ACME CORP", "123 Industrial Way, Springfield", "",
			"TAX INVOICE", "Invoice Number: AC-2026-0184", "Invoice Date: 12/03/2026",
			"Due Date: 11/04/2026", "", "Bill To: Alex Rao", "",
			"Consulting services, February 2026 .............. 2,400.00",
			"Platform subscription ........................... 180.00", "",
			"Total: 2580.00", "", "Payment received with thanks.",
		},
	},
	{
		DocType: "receipt", Category: "financial", Owners: []string{"Sam Rao"},
		Identifier: "Globex", Year: 2025, Company: "globex", Lifecycle: "active",
		Body: []string{
			"GLOBEX RETAIL", "", "TRANSACTION RECEIPT", "Receipt Number: GX-88213",
			"Date: 04/11/2025", "", "Office chair ............... 210.00",
			"Desk lamp ..................  38.50", "", "Total: 248.50",
			"", "Payment received. Thank you for your payment.",
		},
	},
	{
		DocType: "statement", Category: "financial", Owners: []string{"Alex Rao"},
		Identifier: "Northwind Bank", Year: 2026, Company: "northwind-bank", Lifecycle: "active",
		Body: []string{
			"NORTHWIND BANK", "", "ACCOUNT STATEMENT",
			"Account Number: XXXX4417", "Statement period: 01/01/2026 to 31/01/2026",
			"", "Opening balance: 4,120.55", "Closing balance: 3,884.10",
		},
	},
	{
		DocType: "payslip", Category: "financial", Owners: []string{"Sam Rao"},
		Identifier: "Initech", Year: 2026, Company: "initech", Lifecycle: "active",
		Extra: []string{"confidential"},
		Body: []string{
			"INITECH LIMITED", "", "PAYSLIP", "Employee ID: IN-2291",
			"Pay period: January 2026", "", "Gross pay: 5,400.00",
			"Deductions: 1,182.00", "Net pay: 4218.00",
		},
	},
	{
		DocType: "tax-return", Category: "financial", Owners: []string{"Alex Rao"},
		Identifier: "Revenue Service", Year: 2025, Areas: []string{"tax"}, Lifecycle: "active",
		Extra: []string{"confidential"},
		Body: []string{
			"REVENUE SERVICE", "", "INCOME TAX RETURN — SELF ASSESSMENT",
			"Tax year: 2025", "Taxpayer: Alex Rao", "", "Taxable income: 78,400.00",
			"Tax paid: 15,120.00",
		},
	},
	{
		DocType: "bill", Category: "utility", Owners: []string{"Alex Rao"},
		Identifier: "City Power", Year: 2026, Company: "city-power", Lifecycle: "active",
		Body: []string{
			"CITY POWER", "", "ELECTRICITY BILL", "Account Number: CP-77401",
			"Billing period: February 2026", "Meter reading: 41,882 kWh",
			"Previous balance: 0.00", "", "Amount due: 96.40", "Due Date: 20/03/2026",
		},
	},
	{
		DocType: "passport", Category: "identity", Owners: []string{"Alex Rao"},
		Identifier: "Passport Office", Year: 2024, Lifecycle: "active",
		Extra: []string{"confidential"},
		Body: []string{
			"PASSPORT", "Type P", "Passport Number: X4820913",
			"Surname: RAO", "Given names: ALEX", "Nationality: Fictional",
			"Place of issue: Springfield", "Date of expiry: 14/06/2034",
			"P<FICRAO<<ALEX<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<",
		},
	},
	{
		DocType: "passport", Category: "identity", Owners: []string{"Alex Rao"},
		Identifier: "Passport Office", Year: 2014, Lifecycle: "superseded",
		Body: []string{
			"PASSPORT", "Type P", "Passport Number: X1180244",
			"Surname: RAO", "Given names: ALEX", "Date of expiry: 09/06/2024",
			"P<FICRAO<<ALEX<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<",
		},
	},
	{
		DocType: "drivers-license", Category: "identity", Owners: []string{"Sam Rao"},
		Identifier: "Transport Authority", Year: 2023, Lifecycle: "active",
		Body: []string{
			"DRIVING LICENCE", "Licence Number: DL-5540-2231",
			"Holder: Sam Rao", "Classes: B", "Date of expiry: 02/09/2033",
		},
	},
	{
		DocType: "boarding-pass", Category: "travel", Owners: []string{"Sam Rao"},
		Identifier: "United Airlines", Year: 2026, Company: "united-airlines", Lifecycle: "active",
		Body: []string{
			"UNITED AIRLINES", "", "BOARDING PASS", "Passenger: RAO/SAM",
			"Flight: UA 214", "Seat: 14C", "Gate: B7", "Boarding time: 08:35",
			"Date: 22/05/2026",
		},
	},
	{
		DocType: "hotel-booking", Category: "travel", Owners: []string{"Alex Rao", "Sam Rao"},
		Identifier: "Grand Hotel", Year: 2026, Company: "grand-hotel", Lifecycle: "active",
		Body: []string{
			"GRAND HOTEL", "", "BOOKING CONFIRMATION",
			"Confirmation number: GH-2026-5512", "Guests: Alex Rao, Sam Rao",
			"Check-in: 22/05/2026", "Check-out: 26/05/2026", "Room: Twin, 2 nights",
		},
	},
	{
		DocType: "insurance-policy", Category: "insurance", Owners: []string{"Alex Rao"},
		Identifier: "Northwind Insurance", Year: 2026, Company: "northwind-insurance",
		Lifecycle: "active",
		Body: []string{
			"NORTHWIND INSURANCE", "", "INSURANCE POLICY",
			"Policy Number: NI-4471-2026", "Insured: Alex Rao",
			"Sum insured: 250,000.00", "Policy period: 01/01/2026 to 31/12/2026",
		},
	},
	{
		DocType: "lease-agreement", Category: "property", Owners: []string{"Alex Rao", "Sam Rao"},
		Identifier: "Maple Street", Year: 2025, Lifecycle: "active",
		Body: []string{
			"LEASE AGREEMENT", "", "Landlord: Maple Street Holdings",
			"Tenants: Alex Rao, Sam Rao", "Premises: 44 Maple Street, Springfield",
			"Term: 24 months from 01/07/2025", "Monthly rent: 1,850.00",
			"Security deposit: 3,700.00",
		},
	},
	{
		DocType: "vehicle-registration", Category: "vehicles", Owners: []string{"Sam Rao"},
		Identifier: "Transport Authority", Year: 2022, Lifecycle: "active",
		Body: []string{
			"CERTIFICATE OF REGISTRATION", "Registration number: SPR-4412",
			"Owner: Sam Rao", "Vehicle: 2021 hatchback", "Chassis number: VIN0099122",
		},
	},
	{
		DocType: "lab-report", Category: "medical", Owners: []string{"Sam Rao"},
		Identifier: "City Clinic", Year: 2026, Lifecycle: "active",
		Extra: []string{"confidential"},
		Body: []string{
			"CITY CLINIC — PATHOLOGY", "", "LABORATORY REPORT",
			"Patient: Sam Rao", "Specimen collected: 03/02/2026",
			"Haemoglobin: 14.1 g/dL (reference range 13.0-17.0)",
		},
	},
	{
		DocType: "contract", Category: "legal", Owners: []string{"Alex Rao"},
		Identifier: "Globex", Year: 2025, Company: "globex", Lifecycle: "active",
		Body: []string{
			"CONSULTING AGREEMENT", "", "This agreement is entered into between",
			"Globex Corporation and Alex Rao.", "Effective date: 01/09/2025",
			"Term: 12 months", "Governing law: State of Springfield",
		},
	},
	{
		DocType: "certificate", Category: "personal", Owners: []string{"Sam Rao"},
		Identifier: "Design Institute", Year: 2021, Lifecycle: "active",
		Body: []string{
			"DESIGN INSTITUTE", "", "CERTIFICATE OF COMPLETION",
			"This is to certify that Sam Rao has successfully completed",
			"the Diploma in Interaction Design.", "Awarded: 18/11/2021",
		},
	},
	{
		DocType: "warranty-card", Category: "personal", Owners: []string{"Alex Rao"},
		Identifier: "Globex", Year: 2024, Company: "globex", Areas: []string{"warranty"},
		Lifecycle: "active",
		Body: []string{
			"GLOBEX", "", "WARRANTY CARD", "Product: Globex 27-inch display",
			"Serial: GX-DSP-88211", "Purchase date: 02/12/2024",
			"Warranty period: 36 months", "Proof of purchase must be retained.",
		},
	},
}

// demoAreaTags is the area vocabulary the demo documents use.
func demoAreaTags() []string { return collectDemoTags(func(d demoDoc) []string { return d.Areas }) }

// demoCompanyTags is the company vocabulary the demo documents use.
func demoCompanyTags() []string {
	return collectDemoTags(func(d demoDoc) []string {
		if d.Company == "" {
			return nil
		}
		return []string{d.Company}
	})
}

// demoFiscalYearTags renders every fiscal-year tag the demo documents carry,
// under the vault's own fiscal calendar. fycal.Year.Tag() is the single source
// of the spelling ("fy2026" for a calendar-year vault, "fy25-26" for a split
// one); hardcoding it here is how a demo vault ends up failing its own lint.
func demoFiscalYearTags(fyStart int) []string {
	cal := demoCalendar(fyStart)
	seen := map[string]bool{}
	var out []string
	for _, d := range demoDocs {
		if d.Year == 0 {
			continue
		}
		tag := cal.YearStarting(d.Year).Tag()
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

// demoCalendar mirrors the label format vaultYAML writes for a start month.
func demoCalendar(fyStart int) fycal.Calendar {
	label := "FY {yyyy1}"
	if fyStart != 1 {
		label = "FY {yy1}-{yy2}"
	}
	return fycal.New(fyStart, label)
}

func collectDemoTags(pick func(demoDoc) []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range demoDocs {
		for _, t := range pick(d) {
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// writeDemoVault materialises the demo documents: a synthetic PDF at its
// conventional path, a sidecar whose source_sha256 matches the bytes on disk,
// and the Finder tags the vault's vocabulary already lists.
//
// It returns the number of documents written and any degradations (a
// filesystem with no extended-attribute support, most often) as warnings
// rather than errors: a demo vault without Finder tags is still a demo vault,
// and Linux CI has no xattrs at all.
func writeDemoVault(ctx context.Context, cfg *config.Config) (int, []string, error) {
	conv, err := conventions.New(cfg)
	if err != nil {
		return 0, nil, err
	}
	catalog, err := doctypes.Resolve(cfg)
	if err != nil {
		return 0, nil, err
	}
	cal := fycal.New(cfg.FiscalYear.StartMonth, cfg.FiscalYear.LabelFormat)
	today := time.Now().Format("2006-01-02")

	var warnings []string
	tagsUnsupported := false
	written := make([]string, 0, len(demoDocs))

	for _, d := range demoDocs {
		if err := ctx.Err(); err != nil {
			return len(written), warnings, err
		}
		if !catalog.Has(d.DocType) {
			warnings = append(warnings, fmt.Sprintf("demo doctype %q is not in the catalog; skipped", d.DocType))
			continue
		}
		path, err := conv.Path(conventions.Doc{
			DocType:    d.DocType,
			Category:   d.Category,
			Owners:     d.Owners,
			Identifier: d.Identifier,
			Year:       d.Year,
			Ext:        ".pdf",
		})
		if err != nil {
			return len(written), warnings, fmt.Errorf("%s: %w", d.DocType, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return len(written), warnings, err
		}
		text := demoText(d)
		if err := os.WriteFile(path, renderPDF(demoTitle(d), d.Body), 0o644); err != nil {
			return len(written), warnings, err
		}
		sum, err := move.SHA256(path)
		if err != nil {
			return len(written), warnings, err
		}
		meta := &sidecar.Meta{
			ExtractedAt: today,
			OCREngine:   "demo",
			Classifier:  "demo",
			DocType:     d.DocType,
			Category:    d.Category,
			Confidence:  1,
			Owners:      d.Owners,
			Identifier:  d.Identifier,
			Year:        d.Year,
			Fields:      demoFields(catalog, d, text),
			SourceSHA:   sum,
			Text:        text,
		}
		if err := sidecar.Write(path, meta); err != nil {
			return len(written), warnings, err
		}
		if !tagsUnsupported {
			if err := tags.Write(path, demoTags(cal, d)); err != nil {
				if errors.Is(err, tags.ErrUnsupported) {
					tagsUnsupported = true
					warnings = append(warnings,
						"this filesystem does not support extended attributes; the demo vault has no Finder tags")
				} else {
					warnings = append(warnings, fmt.Sprintf("%s: tags not applied: %v", filepath.Base(path), err))
				}
			}
		}
		written = append(written, path)
	}

	if len(written) > 0 {
		log := audit.Open(cfg.AuditLogPath())
		if err := log.Append(audit.Entry{
			Op:     "init-demo",
			Paths:  written,
			Detail: map[string]string{"documents": strconv.Itoa(len(written))},
		}); err != nil {
			warnings = append(warnings, fmt.Sprintf("audit line not written: %v", err))
		}
	}
	return len(written), warnings, nil
}

// demoTags assembles a document's Finder tags from the same table the vault's
// vocabulary was written from.
func demoTags(cal fycal.Calendar, d demoDoc) []string {
	var out []string
	for _, owner := range d.Owners {
		out = append(out, config.Slug(owner))
	}
	if d.Company != "" {
		out = append(out, d.Company)
	}
	out = append(out, d.Areas...)
	if d.Year > 0 {
		out = append(out, cal.YearStarting(d.Year).Tag())
	}
	if d.Lifecycle != "" {
		out = append(out, d.Lifecycle)
	}
	out = append(out, d.Extra...)
	return tags.Normalize(out)
}

// demoFields runs the real extraction templates over the demo text, so the
// sidecars hold fields that were genuinely extracted rather than transcribed.
func demoFields(catalog *doctypes.Catalog, d demoDoc, text string) map[string]string {
	fields := catalog.ExtractFields(d.DocType, text)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func demoText(d demoDoc) string {
	out := ""
	for i, line := range d.Body {
		if i > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}

func demoTitle(d demoDoc) string {
	return conventions.TitleCase(d.DocType) + " — " + d.Identifier
}
