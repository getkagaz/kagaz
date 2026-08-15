package models

import (
	"fmt"
	"strings"
)

// LicenseNote is the informational note printed before a pull.
//
// It is informational and never a gate (docs/model-use.md): Kagaz vendors no
// weights and redistributes none, so it is a downstream user of a model rather
// than a distributor, and a document manager has no basis to adjudicate a
// model's license on the user's behalf. Reading the license is the user's
// responsibility; the note's job is to make sure they know there is one and
// where to find it.
func LicenseNote(repo string) string {
	return fmt.Sprintf(`Model: %s
License: set by whoever published these weights, not by Kagaz. Kagaz bundles,
  vendors and redistributes no model weights; it is downloading them on your
  behalf as a downstream user.
  Read the terms at %s
This note is informational. It does not gate the download, and Kagaz does not
check or enforce model licenses -- that is yours to do, exactly as it would be
if you installed the model any other way.`, repo, ModelPage(repo))
}

// ModelPage is the published page for a repo, where its license lives.
func ModelPage(repo string) string {
	return DefaultEndpoint + "/" + repo
}

// OllamaLicenseNote is the equivalent note for a model pulled through the local
// Ollama daemon.
//
// The download itself is Ollama's network activity rather than Kagaz's, but the
// user is acquiring a third-party model because Kagaz asked them to, so the
// same informational note applies -- and, exactly as on the MLX path, it gates
// nothing.
func OllamaLicenseNote(model string) string {
	where := "consult the model's publisher for its license terms"
	if page, ok := ollamaLibraryPage(model); ok {
		where = "Read the terms at " + page
	}
	return fmt.Sprintf(`Model: %s (pulled by your local Ollama daemon, not by Kagaz)
License: set by whoever published these weights, not by Kagaz. Kagaz bundles,
  vendors and redistributes no model weights.
  %s
This note is informational. It does not gate the pull, and Kagaz does not check
or enforce model licenses -- that is yours to do, exactly as it would be if you
ran `+"`ollama pull`"+` yourself.`, model, where)
}

// ollamaLibraryPage returns the ollama.com page for a plain library model name
// ("llama3.2", "llama3.2:3b"). Models pulled from another registry
// ("hf.co/org/name") have no page here, and no link is invented for them.
func ollamaLibraryPage(model string) (string, bool) {
	name := strings.TrimSpace(model)
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return "https://ollama.com/library/" + name, true
}
