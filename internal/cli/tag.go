package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
	"github.com/spf13/cobra"
)

// TagChange is one document's tag transition. It is shared by `tag` and
// `supersede` so both report the same shape.
type TagChange struct {
	Path    string   `json:"path"`
	Before  []string `json:"before"`
	After   []string `json:"after"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// TagPayload is the `kagaz tag --json` body.
type TagPayload struct {
	Changes []TagChange `json:"changes"`
	// Unvalidated lists tags applied outside the controlled vocabulary because
	// --force was given. They are named rather than hidden: each one is a
	// `kagaz lint` finding from the moment it lands.
	Unvalidated []string `json:"unvalidated,omitempty"`
	Forced      bool     `json:"forced"`
}

func newTagCommand(rt *Runtime) *cobra.Command {
	var (
		add    []string
		remove []string
		force  bool
		mut    mutationFlags
	)

	cmd := &cobra.Command{
		Use:   "tag <path>...",
		Short: "Add or remove Finder tags on a document",
		Long: "tag edits a document's Finder tags, validated against the vault's controlled\n" +
			"vocabulary. --force applies a tag outside the vocabulary anyway, which is a\n" +
			"`kagaz lint` finding from the moment it lands.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			if len(add) == 0 && len(remove) == 0 {
				return errors.New("nothing to do: pass --add and/or --remove")
			}
			add = tags.Normalize(add)
			remove = tags.Normalize(remove)

			vocab := tags.NewVocabulary(cfg)
			unknown := vocab.Unknown(add)
			if len(unknown) > 0 && !force {
				return fmt.Errorf("tag(s) %s are not in this vault's vocabulary; add them to tags: in vault.yaml, or pass --force",
					strings.Join(unknown, ", "))
			}

			payload := TagPayload{Forced: force, Changes: []TagChange{}}
			if force {
				payload.Unvalidated = unknown
			}
			for _, arg := range args {
				path, err := documentPath(cfg, arg)
				if err != nil {
					return err
				}
				before, err := tags.Read(path)
				if err != nil && !errors.Is(err, tags.ErrUnsupported) {
					return err
				}
				payload.Changes = append(payload.Changes, TagChange{
					Path:    path,
					Before:  before,
					After:   applyTagSet(before, add, remove),
					Added:   missing(before, add),
					Removed: present(before, remove),
				})
			}

			approved, resp := mut.approve(rt, "tag", payload, previewTag)
			if !approved {
				return rt.Emit(resp)
			}

			var warnings []string
			for _, ch := range payload.Changes {
				if err := tags.Apply(ch.Path, add, remove); err != nil {
					if errors.Is(err, tags.ErrUnsupported) {
						warnings = append(warnings,
							fmt.Sprintf("%s: %v (Finder tags are unavailable on this filesystem)", filepath.Base(ch.Path), err))
						continue
					}
					return err
				}
			}
			log := rt.Audit(cfg)
			paths := make([]string, 0, len(payload.Changes))
			for _, ch := range payload.Changes {
				paths = append(paths, ch.Path)
			}
			if err := log.Append(audit.Entry{
				Op: "tag", Paths: paths,
				Detail: map[string]string{"added": strings.Join(add, ","), "removed": strings.Join(remove, ",")},
			}); err != nil {
				warnings = append(warnings, fmt.Sprintf("audit line not written: %v", err))
			}

			return rt.Emit(&Response{
				Command: "tag", Status: StatusOK, Payload: payload,
				Warnings: warnings, Human: humanTag,
			})
		},
	}

	f := cmd.Flags()
	f.StringSliceVar(&add, "add", nil, "tag to add (repeatable)")
	f.StringSliceVar(&remove, "remove", nil, "tag to remove (repeatable)")
	f.BoolVar(&force, "force", false, "apply tags outside the controlled vocabulary")
	mut.register(cmd)
	return cmd
}

// SupersedePayload is the `kagaz supersede --json` body.
type SupersedePayload struct {
	// Superseded is the document being retired.
	Superseded TagChange `json:"superseded"`
	// Current is the document taking its place.
	Current TagChange `json:"current"`
}

func newSupersedeCommand(rt *Runtime) *cobra.Command {
	var mut mutationFlags

	cmd := &cobra.Command{
		Use:   "supersede <old> <new>",
		Short: "Mark a document as replaced by a newer one",
		Long: "supersede moves the lifecycle tag from active to superseded on the old\n" +
			"document and applies active to the new one, in one operation, so the\n" +
			"single_active_per_doctype_per_person rule is never transiently violated by\n" +
			"the very command meant to satisfy it.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			oldPath, err := documentPath(cfg, args[0])
			if err != nil {
				return err
			}
			newPath, err := documentPath(cfg, args[1])
			if err != nil {
				return err
			}
			if oldPath == newPath {
				return errors.New("a document cannot supersede itself")
			}

			oldBefore, err := readTagsTolerant(oldPath)
			if err != nil {
				return err
			}
			newBefore, err := readTagsTolerant(newPath)
			if err != nil {
				return err
			}
			payload := SupersedePayload{
				Superseded: TagChange{
					Path:    oldPath,
					Before:  oldBefore,
					After:   applyTagSet(oldBefore, []string{"superseded"}, []string{"active"}),
					Added:   missing(oldBefore, []string{"superseded"}),
					Removed: present(oldBefore, []string{"active"}),
				},
				Current: TagChange{
					Path:    newPath,
					Before:  newBefore,
					After:   applyTagSet(newBefore, []string{"active"}, []string{"superseded"}),
					Added:   missing(newBefore, []string{"active"}),
					Removed: present(newBefore, []string{"superseded"}),
				},
			}

			approved, resp := mut.approve(rt, "supersede", payload, previewSupersede)
			if !approved {
				return rt.Emit(resp)
			}

			var warnings []string
			apply := func(path string, add, remove []string) {
				if err := tags.Apply(path, add, remove); err != nil {
					warnings = append(warnings, fmt.Sprintf("%s: tags not applied: %v", filepath.Base(path), err))
				}
			}
			apply(oldPath, []string{"superseded"}, []string{"active"})
			apply(newPath, []string{"active"}, []string{"superseded"})

			log := rt.Audit(cfg)
			if err := log.Append(audit.Entry{
				Op:    "supersede",
				Paths: []string{oldPath, newPath},
				Detail: map[string]string{
					"superseded": oldPath,
					"current":    newPath,
				},
			}); err != nil {
				warnings = append(warnings, fmt.Sprintf("audit line not written: %v", err))
			}

			return rt.Emit(&Response{
				Command: "supersede", Status: StatusOK, Payload: payload,
				Warnings: warnings, Human: humanSupersede,
			})
		},
	}
	mut.register(cmd)
	return cmd
}

// documentPath resolves a user-supplied document argument to an absolute path,
// accepting either a filesystem path or a path relative to the vault root.
func documentPath(cfg *config.Config, arg string) (string, error) {
	candidates := []string{arg}
	if !filepath.IsAbs(arg) {
		candidates = append(candidates, filepath.Join(cfg.VaultRoot, filepath.FromSlash(arg)))
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(config.ExpandHome(c))
		if err != nil {
			continue
		}
		if st, err := os.Stat(abs); err == nil && !st.IsDir() {
			return abs, nil
		}
	}
	return "", fmt.Errorf("no such document: %s", arg)
}

// readTagsTolerant reads Finder tags, treating a filesystem without extended
// attributes as "no tags" rather than as a failure.
func readTagsTolerant(path string) ([]string, error) {
	list, err := tags.Read(path)
	if err != nil && !errors.Is(err, tags.ErrUnsupported) {
		return nil, err
	}
	if list == nil {
		list = []string{}
	}
	return list, nil
}

// applyTagSet computes the resulting tag set without touching the filesystem,
// so a preview and the write that follows describe the same outcome.
func applyTagSet(before, add, remove []string) []string {
	set := map[string]bool{}
	for _, t := range before {
		set[t] = true
	}
	for _, t := range remove {
		delete(set, t)
	}
	for _, t := range add {
		set[t] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// missing returns the tags in want that are not already present.
func missing(have, want []string) []string {
	set := map[string]bool{}
	for _, t := range have {
		set[t] = true
	}
	var out []string
	for _, t := range want {
		if !set[t] {
			out = append(out, t)
		}
	}
	return out
}

// present returns the tags in want that are currently present.
func present(have, want []string) []string {
	set := map[string]bool{}
	for _, t := range have {
		set[t] = true
	}
	var out []string
	for _, t := range want {
		if set[t] {
			out = append(out, t)
		}
	}
	return out
}

func previewTag(w io.Writer, payload any) error {
	p, ok := payload.(TagPayload)
	if !ok {
		return fmt.Errorf("tag: unexpected payload %T", payload)
	}
	for _, ch := range p.Changes {
		fmt.Fprintf(w, "%s\n  %s -> %s\n", ch.Path, joinOrNone(ch.Before), joinOrNone(ch.After))
	}
	if len(p.Unvalidated) > 0 {
		fmt.Fprintf(w, "\n--force: %s are outside the vocabulary and will be lint findings\n",
			strings.Join(p.Unvalidated, ", "))
	}
	return nil
}

func humanTag(w io.Writer, payload any) error {
	p, ok := payload.(TagPayload)
	if !ok {
		return fmt.Errorf("tag: unexpected payload %T", payload)
	}
	for _, ch := range p.Changes {
		fmt.Fprintf(w, "%s: %s\n", filepath.Base(ch.Path), joinOrNone(ch.After))
	}
	return nil
}

func previewSupersede(w io.Writer, payload any) error {
	p, ok := payload.(SupersedePayload)
	if !ok {
		return fmt.Errorf("supersede: unexpected payload %T", payload)
	}
	fmt.Fprintf(w, "superseded: %s\n  %s -> %s\n", p.Superseded.Path,
		joinOrNone(p.Superseded.Before), joinOrNone(p.Superseded.After))
	fmt.Fprintf(w, "current:    %s\n  %s -> %s\n", p.Current.Path,
		joinOrNone(p.Current.Before), joinOrNone(p.Current.After))
	return nil
}

func humanSupersede(w io.Writer, payload any) error {
	p, ok := payload.(SupersedePayload)
	if !ok {
		return fmt.Errorf("supersede: unexpected payload %T", payload)
	}
	fmt.Fprintf(w, "%s is now superseded\n", filepath.Base(p.Superseded.Path))
	fmt.Fprintf(w, "%s is now active\n", filepath.Base(p.Current.Path))
	return nil
}

func joinOrNone(list []string) string {
	if len(list) == 0 {
		return "(no tags)"
	}
	return strings.Join(list, ", ")
}

// sortedKeys returns a map's keys in a stable order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
