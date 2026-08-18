package cli

import (
	"fmt"
	"io"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/models"
	"github.com/spf13/cobra"
)

// ModelPullPayload is the `kagaz model pull --json` body.
type ModelPullPayload struct {
	// Engine is "mlx" (Hugging Face weights) or "ollama" (local daemon pull).
	Engine string `json:"engine"`
	// Repo is the model identifier that was pulled.
	Repo string `json:"repo"`
	// Revision is the pinned revision, for the mlx engine.
	Revision string `json:"revision,omitempty"`
	// Dir is where the weights now live.
	Dir string `json:"dir,omitempty"`
	// Downloaded and Reused split the manifest by what this run had to fetch.
	Downloaded []string `json:"downloaded,omitempty"`
	Reused     []string `json:"reused,omitempty"`
	// AlreadyReady reports a no-op re-run.
	AlreadyReady bool `json:"already_ready"`
	// License is the informational licence note for the chosen model. It is
	// printed, never enforced.
	License string `json:"license,omitempty"`
}

func newModelCommand(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Manage on-device model weights",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newModelPullCommand(rt))
	return cmd
}

func newModelPullCommand(rt *Runtime) *cobra.Command {
	var (
		engine   string
		revision string
		force    bool
	)

	cmd := &cobra.Command{
		Use:   "pull [model]",
		Short: "Download model weights (the only command that reaches the network)",
		Long: "pull downloads MLX weights from a pinned Hugging Face repo into the local\n" +
			"model cache, resumable and SHA256-verified per file; the model is marked ready\n" +
			"only once every file checks out. --engine ollama delegates the pull to your\n" +
			"local Ollama daemon instead. The licence note is informational: reading the\n" +
			"model's licence is your responsibility, not Kagaz's to enforce.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			model := ""
			if len(args) == 1 {
				model = args[0]
			}
			if model == "" {
				if cfg, err := rt.Config(); err == nil && cfg.Classify.Model != "" {
					model = cfg.Classify.Model
				} else {
					model = config.DefaultMLXModel
				}
			}

			switch engine {
			case config.EngineOllama:
				payload := ModelPullPayload{Engine: config.EngineOllama, Repo: model,
					License: models.OllamaLicenseNote(model)}
				endpoint := "http://localhost:11434"
				if cfg, err := rt.Config(); err == nil && cfg.Classify.Endpoint != "" {
					endpoint = cfg.Classify.Endpoint
				}
				puller := &models.OllamaPuller{Endpoint: endpoint, Log: func(line string) { rt.Printf("%s\n", line) }}
				if err := puller.Pull(cmd.Context(), model, func(line string) { rt.Printf("%s\n", line) }); err != nil {
					return err
				}
				return rt.Emit(&Response{Command: "model pull", Status: StatusOK, Payload: payload, Human: humanModelPull})

			case "", config.EngineMLX:
				client := &models.Client{}
				res, err := client.Pull(cmd.Context(), models.Options{
					Repo:     model,
					Revision: revision,
					Force:    force,
					Log:      func(line string) { rt.Printf("%s\n", line) },
				})
				if err != nil {
					return err
				}
				return rt.Emit(&Response{
					Command: "model pull", Status: StatusOK, Human: humanModelPull,
					Payload: ModelPullPayload{
						Engine: config.EngineMLX, Repo: res.Repo, Revision: res.Revision, Dir: res.Dir,
						Downloaded: res.Downloaded, Reused: res.Reused, AlreadyReady: res.AlreadyReady,
						License: models.LicenseNote(res.Repo),
					},
				})

			default:
				return fmt.Errorf("--engine must be %q or %q, got %q", config.EngineMLX, config.EngineOllama, engine)
			}
		},
	}

	f := cmd.Flags()
	f.StringVar(&engine, "engine", "", "mlx (default) or ollama")
	f.StringVar(&revision, "revision", "", "override the pinned revision (mlx only)")
	f.BoolVar(&force, "force", false, "re-download and re-verify even when already ready")
	return cmd
}

func humanModelPull(w io.Writer, payload any) error {
	p, ok := payload.(ModelPullPayload)
	if !ok {
		return fmt.Errorf("model pull: unexpected payload %T", payload)
	}
	if p.AlreadyReady {
		fmt.Fprintf(w, "%s is already ready at %s\n", p.Repo, p.Dir)
	} else {
		fmt.Fprintf(w, "%s pulled (%s)\n", p.Repo, p.Engine)
		if p.Dir != "" {
			fmt.Fprintf(w, "  %s\n", p.Dir)
		}
		if len(p.Downloaded) > 0 {
			fmt.Fprintf(w, "  %d file(s) downloaded, %d reused\n", len(p.Downloaded), len(p.Reused))
		}
	}
	if p.License != "" {
		fmt.Fprintf(w, "\n%s\n", p.License)
	}
	return nil
}
