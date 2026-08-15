// Package keychain is a thin wrapper over the macOS `security` tool. It exists
// so that Kagaz can record *which* Keychain item unlocks an encrypted document
// without ever handling the password itself.
//
// Safety invariant 5: no function here returns a secret value. Item names go in
// sidecars and lint output; passwords never do.
package keychain

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// ServicePrefix namespaces Kagaz's Keychain items.
const ServicePrefix = "kagaz:"

// ErrUnavailable means the `security` tool is not present (non-macOS).
var ErrUnavailable = errors.New("the macOS `security` tool is unavailable")

// Available reports whether Keychain access is possible on this machine.
func Available() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath("security")
	return err == nil
}

// ItemName is the conventional Keychain service name for a document.
func ItemName(label string) string { return ServicePrefix + label }

// Exists reports whether a generic-password item with the given name is
// present. It deliberately uses the metadata-only form of `find-generic-
// password`: without -w, the tool prints attributes and never the secret.
func Exists(name string) (bool, error) {
	if !Available() {
		return false, ErrUnavailable
	}
	cmd := exec.Command("security", "find-generic-password", "-s", name)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = nil
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Exit 44 is "the specified item could not be found".
			if exitErr.ExitCode() == 44 || strings.Contains(stderr.String(), "could not be found") {
				return false, nil
			}
		}
		return false, fmt.Errorf("security: %s", strings.TrimSpace(stderr.String()))
	}
	return true, nil
}

// List returns the Kagaz-namespaced item names in the login keychain. Only
// names are returned; values are never read.
func List() ([]string, error) {
	if !Available() {
		return nil, ErrUnavailable
	}
	out, err := exec.Command("security", "dump-keychain").Output()
	if err != nil {
		return nil, fmt.Errorf("security dump-keychain: %w", err)
	}
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// Service attribute lines look like: "svce"<blob>="kagaz:acme-invoice"
		if !strings.HasPrefix(line, `"svce"`) {
			continue
		}
		i := strings.Index(line, `="`)
		if i < 0 {
			continue
		}
		name := strings.TrimSuffix(line[i+2:], `"`)
		if strings.HasPrefix(name, ServicePrefix) && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}
