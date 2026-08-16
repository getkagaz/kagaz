package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/ingest"
	"github.com/getkagaz/kagaz/internal/vaultkit/sidecar"
	"github.com/spf13/cobra"
)

// WatchPayload is one debounced batch of proposals, emitted as one JSON
// envelope per batch (JSON Lines) so a long-running watcher stays streamable.
type WatchPayload struct {
	// Watching lists the directories under watch.
	Watching []string `json:"watching,omitempty"`
	// Trigger names the files whose change produced this batch.
	Trigger []string `json:"trigger,omitempty"`
	// Proposals are what ingest would do. watch never executes them.
	Proposals []ingest.Proposal `json:"proposals"`
}

func newWatchCommand(rt *Runtime) *cobra.Command {
	var (
		debounce time.Duration
		once     bool
	)

	cmd := &cobra.Command{
		Use:   "watch [directory]...",
		Short: "Watch for new files and propose a home for them",
		Long: "watch reacts to new and changed files (fsnotify, debounced) by running the\n" +
			"ingest pipeline's propose stage. It never executes a move: proposals are\n" +
			"printed for you to act on with `kagaz ingest`. Meant to run under\n" +
			"`brew services start kagaz`.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := rt.Config()
			if err != nil {
				return err
			}
			roots := args
			if len(roots) == 0 {
				roots = []string{cfg.VaultRoot}
			}
			pipeline, err := ingest.New(cfg)
			if err != nil {
				return err
			}
			return watchLoop(cmd.Context(), rt, cfg, pipeline, roots, debounce, once)
		},
	}

	cmd.Flags().DurationVar(&debounce, "debounce", 2*time.Second, "how long to wait for a burst of changes to settle")
	cmd.Flags().BoolVar(&once, "once", false, "propose for the first settled batch, then exit")
	return cmd
}

// watchLoop is the watcher proper, factored out so it can be driven by a test
// with a cancellable context.
func watchLoop(ctx context.Context, rt *Runtime, cfg *config.Config, pipeline *ingest.Pipeline,
	roots []string, debounce time.Duration, once bool) error {

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	var watching []string
	for _, root := range roots {
		abs, err := filepath.Abs(config.ExpandHome(root))
		if err != nil {
			return err
		}
		if err := addWatchTree(watcher, cfg, abs, &watching); err != nil {
			return err
		}
	}
	if len(watching) == 0 {
		return fmt.Errorf("nothing to watch under %s", strings.Join(roots, ", "))
	}

	if err := rt.Emit(&Response{
		Command: "watch", Status: StatusOK,
		Payload: WatchPayload{Watching: watching, Proposals: []ingest.Proposal{}},
		Human: func(w io.Writer, payload any) error {
			p, ok := payload.(WatchPayload)
			if !ok {
				return fmt.Errorf("watch: unexpected payload %T", payload)
			}
			fmt.Fprintf(w, "watching %d director(ies); proposals only, nothing will be moved\n", len(p.Watching))
			return nil
		},
	}); err != nil {
		return err
	}

	pending := map[string]bool{}
	var timer *time.Timer
	var fire <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
				continue
			}
			if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
				// A new directory appears inside a watched tree; follow it, so
				// dropping a whole folder into the vault is noticed.
				_ = addWatchTree(watcher, cfg, ev.Name, &watching)
				continue
			}
			if !watchable(cfg, ev.Name) {
				continue
			}
			pending[ev.Name] = true
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(debounce)
			fire = timer.C

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			rt.Warnf("watch: %v\n", err)

		case <-fire:
			fire = nil
			paths := make([]string, 0, len(pending))
			for p := range pending {
				if _, err := os.Stat(p); err == nil {
					paths = append(paths, p)
				}
			}
			pending = map[string]bool{}
			if len(paths) == 0 {
				continue
			}
			proposals, err := pipeline.Analyze(ctx, paths)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				rt.Warnf("watch: %v\n", err)
				continue
			}
			if proposals == nil {
				proposals = []ingest.Proposal{}
			}
			if err := rt.Emit(&Response{
				Command: "watch", Status: StatusProposed,
				Payload: WatchPayload{Trigger: paths, Proposals: proposals},
				Human:   humanWatch,
			}); err != nil {
				return err
			}
			if once {
				return nil
			}
		}
	}
}

// addWatchTree registers dir and every subdirectory that is not Kagaz's own
// bookkeeping.
func addWatchTree(watcher *fsnotify.Watcher, cfg *config.Config, dir string, watching *[]string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable directory is not worth failing the watcher over
		}
		if !d.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		if path != dir && (strings.HasPrefix(name, ".") || path == cfg.ManifestDir() || path == cfg.StagingDir()) {
			return fs.SkipDir
		}
		if err := watcher.Add(path); err == nil {
			*watching = append(*watching, path)
		}
		return nil
	})
}

// watchable filters out the files a proposal could never be about.
func watchable(cfg *config.Config, path string) bool {
	name := filepath.Base(path)
	if strings.HasPrefix(name, ".") || sidecar.IsSidecar(path) {
		return false
	}
	if name == config.FileName || name == "INDEX.md" || name == "AGENTS.md" {
		return false
	}
	if name == filepath.Base(cfg.AuditLogPath()) {
		return false
	}
	for _, reserved := range []string{cfg.ManifestDir(), cfg.StagingDir()} {
		if rel, err := filepath.Rel(reserved, path); err == nil && !strings.HasPrefix(rel, "..") {
			return false
		}
	}
	return true
}

func humanWatch(w io.Writer, payload any) error {
	p, ok := payload.(WatchPayload)
	if !ok {
		return fmt.Errorf("watch: unexpected payload %T", payload)
	}
	fmt.Fprintf(w, "\n%d change(s) settled; proposing (nothing is moved):\n", len(p.Trigger))
	for _, prop := range p.Proposals {
		fmt.Fprintf(w, "  %s\n", summarizeProposal(prop))
	}
	fmt.Fprintln(w, "  run `kagaz ingest <path>` to review and file these")
	return nil
}
