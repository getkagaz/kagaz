package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/search"
	"github.com/spf13/cobra"
)

// FindQuery echoes the filters a find ran with, so a JSON consumer can tell
// which query produced a result set without re-parsing the command line.
type FindQuery struct {
	Text    string   `json:"text,omitempty"`
	Person  string   `json:"person,omitempty"`
	Company string   `json:"company,omitempty"`
	Area    string   `json:"area,omitempty"`
	DocType string   `json:"doctype,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Active  bool     `json:"active,omitempty"`
	Period  string   `json:"period,omitempty"`
}

// FindResult is one document as `kagaz find --json` reports it. It is the
// shape the MCP `find` tool and the menu-bar app both consume.
type FindResult struct {
	Path       string   `json:"path"`
	RelPath    string   `json:"rel_path"`
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	DocType    string   `json:"doctype"`
	Owners     []string `json:"owners,omitempty"`
	Identifier string   `json:"identifier,omitempty"`
	Year       int      `json:"year,omitempty"`
	Modifier   string   `json:"modifier,omitempty"`
	Tags       []string `json:"tags"`
	Parsed     bool     `json:"parsed"`
	HasSidecar bool     `json:"has_sidecar"`
	Evicted    bool     `json:"evicted"`
	Size       int64    `json:"size"`
	ModTime    string   `json:"mod_time,omitempty"`
}

// FindPayload is the `kagaz find --json` body.
type FindPayload struct {
	VaultRoot string       `json:"vault_root"`
	Query     FindQuery    `json:"query"`
	Count     int          `json:"count"`
	Truncated bool         `json:"truncated"`
	Results   []FindResult `json:"results"`
}

func newFindCommand(rt *Runtime) *cobra.Command {
	var q FindQuery
	var limit int

	cmd := &cobra.Command{
		Use:   "find [query]",
		Short: "Query the vault (read-only)",
		Long: "find is the read-only query command. Filters combine with AND; a bare\n" +
			"positional query is matched against the filename, path, sidecar text and\n" +
			"extracted fields.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			q.Text = strings.TrimSpace(strings.Join(args, " "))
			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			searcher, err := search.New(cfg)
			if err != nil {
				return err
			}
			searcher.Spotlight = search.NewMDFind()

			docs, err := searcher.Find(cmd.Context(), search.Query{
				Text:    q.Text,
				Person:  q.Person,
				Company: q.Company,
				Area:    q.Area,
				DocType: q.DocType,
				Tags:    q.Tags,
				Active:  q.Active,
				Period:  q.Period,
			})
			if err != nil {
				return err
			}

			payload := FindPayload{VaultRoot: cfg.VaultRoot, Query: q, Count: len(docs), Results: []FindResult{}}
			var warnings []string
			for i := range docs {
				if limit > 0 && len(payload.Results) >= limit {
					payload.Truncated = true
					break
				}
				payload.Results = append(payload.Results, toFindResult(&docs[i]))
			}
			for i := range docs {
				if docs[i].TagsUnsupported {
					warnings = append(warnings, "this filesystem does not support extended attributes; no Finder tags could be read")
					break
				}
			}

			return rt.Emit(&Response{
				Command:  "find",
				Status:   StatusOK,
				Payload:  payload,
				Warnings: warnings,
				Human:    humanFind,
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&q.Person, "person", "", "person display name or tag")
	f.StringVar(&q.Company, "company", "", "company tag from the vocabulary")
	f.StringVar(&q.Area, "area", "", "area tag from the vocabulary")
	f.StringVar(&q.DocType, "doctype", "", "catalog doctype name")
	f.StringSliceVar(&q.Tags, "tag", nil, "Finder tag that must be present (repeatable)")
	f.BoolVar(&q.Active, "active", false, "only documents tagged active")
	f.StringVar(&q.Period, "period", "", "calendar or fiscal period, e.g. 2026, FY2026, FY2026Q3")
	f.IntVar(&limit, "limit", 0, "stop after this many results (0 means no limit)")
	return cmd
}

// toFindResult flattens a search.Document into the reported shape.
func toFindResult(d *search.Document) FindResult {
	r := FindResult{
		Path:       d.Path,
		RelPath:    d.RelPath,
		Name:       d.Name,
		Category:   d.Category,
		DocType:    d.DocType(),
		Owners:     d.Owners(),
		Identifier: d.Doc.Identifier,
		Year:       d.Year(),
		Modifier:   d.Doc.Modifier,
		Tags:       d.Tags,
		Parsed:     d.Parsed,
		HasSidecar: d.HasSidecar,
		Evicted:    d.Evicted,
		Size:       d.Size,
	}
	if r.Tags == nil {
		r.Tags = []string{}
	}
	if !d.ModTime.IsZero() {
		r.ModTime = d.ModTime.UTC().Format(time.RFC3339)
	}
	return r
}

// humanFind renders the same payload the JSON envelope carries.
func humanFind(w io.Writer, payload any) error {
	p, ok := payload.(FindPayload)
	if !ok {
		return fmt.Errorf("find: unexpected payload %T", payload)
	}
	if p.Count == 0 {
		fmt.Fprintln(w, "no documents matched")
		return nil
	}
	for _, r := range p.Results {
		fmt.Fprintf(w, "%s\n", r.RelPath)
		detail := []string{r.DocType}
		if len(r.Owners) > 0 {
			detail = append(detail, strings.Join(r.Owners, "+"))
		}
		if r.Year > 0 {
			detail = append(detail, fmt.Sprintf("%d", r.Year))
		}
		if len(r.Tags) > 0 {
			detail = append(detail, "tags: "+strings.Join(r.Tags, ", "))
		}
		if r.Evicted {
			detail = append(detail, "iCloud placeholder (not downloaded)")
		}
		fmt.Fprintf(w, "    %s\n", strings.Join(detail, "  |  "))
	}
	if p.Truncated {
		fmt.Fprintf(w, "\n%d shown of %d matches (--limit)\n", len(p.Results), p.Count)
	} else {
		fmt.Fprintf(w, "\n%d document(s)\n", p.Count)
	}
	return nil
}
