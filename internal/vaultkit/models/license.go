package models

import "fmt"

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
