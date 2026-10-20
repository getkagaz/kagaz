package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/userconfig"
)

// settingsIn points the user settings file at a temp directory for one test,
// so nothing here can read or write the real one.
func settingsIn(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(userconfig.EnvDir, dir)
	return filepath.Join(dir, userconfig.FileName)
}

// initVault creates an empty vault and returns its vault.yaml path.
func initVault(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if out, errw, code := run(t, "init", "--root", root); code != ExitOK {
		t.Fatalf("init exit %d\nstdout: %s\nstderr: %s", code, out, errw)
	}
	return filepath.Join(root, "vault.yaml")
}

func TestUseSavesAnAbsoluteVaultPath(t *testing.T) {
	settings := settingsIn(t)
	vault := initVault(t, "saved")

	out, errw, code := run(t, "use", vault)
	if code != ExitOK {
		t.Fatalf("use exit %d: %s", code, errw)
	}
	if !strings.Contains(out, vault) {
		t.Errorf("use did not report the vault it saved: %s", out)
	}

	saved, err := userconfig.Load()
	if err != nil {
		t.Fatalf("loading settings: %v", err)
	}
	if saved.Vault != vault {
		t.Errorf("saved %q, want %q", saved.Vault, vault)
	}
	if _, err := os.Stat(settings); err != nil {
		t.Errorf("the settings file was not written: %v", err)
	}
}

// A relative path saved as typed would mean a different vault from every
// directory it was later read in.
func TestUseStoresARelativePathAsAbsolute(t *testing.T) {
	settingsIn(t)
	vault := initVault(t, "relative")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Dir(filepath.Dir(vault))); err != nil {
		t.Fatal(err)
	}

	if _, errw, code := run(t, "use", "relative"); code != ExitOK {
		t.Fatalf("use exit %d: %s", code, errw)
	}
	saved, err := userconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(saved.Vault) {
		t.Errorf("saved a relative path: %q", saved.Vault)
	}
}

// Saving a path that is not a vault would defer the failure to every later
// command, each reporting it far from the decision that caused it.
func TestUseRefusesSomethingThatIsNotAVault(t *testing.T) {
	settingsIn(t)
	_, errw, code := run(t, "use", t.TempDir())
	if code == ExitOK {
		t.Fatal("use accepted a directory with no vault.yaml")
	}
	if !strings.Contains(errw, "is not a vault") {
		t.Errorf("the error does not say what was wrong: %s", errw)
	}
	if saved, _ := userconfig.Load(); saved.Vault != "" {
		t.Errorf("a rejected path was saved anyway: %q", saved.Vault)
	}
}

func TestUseWithNoArgumentReportsWhatIsSaved(t *testing.T) {
	settingsIn(t)
	out, _, _ := run(t, "use")
	if !strings.Contains(out, "no vault saved") {
		t.Errorf("an unset vault was not reported as unset: %s", out)
	}

	vault := initVault(t, "reported")
	run(t, "use", vault)
	out, _, code := run(t, "use")
	if code != ExitOK {
		t.Fatalf("use exit %d", code)
	}
	if !strings.Contains(out, vault) {
		t.Errorf("use did not report the saved vault: %s", out)
	}
}

func TestUseClearForgetsTheSavedVault(t *testing.T) {
	settingsIn(t)
	vault := initVault(t, "cleared")
	run(t, "use", vault)

	if _, errw, code := run(t, "use", "--clear"); code != ExitOK {
		t.Fatalf("use --clear exit %d: %s", code, errw)
	}
	if saved, _ := userconfig.Load(); saved.Vault != "" {
		t.Errorf("--clear left %q saved", saved.Vault)
	}
}

// The point of the whole feature: a command run from a directory inside no
// vault finds the one the app (or `kagaz use`) chose.
func TestASavedVaultIsUsedFromOutsideAnyVault(t *testing.T) {
	settingsIn(t)
	vault := initVault(t, "elsewhere")
	run(t, "use", vault)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(t.TempDir()); err != nil { // inside no vault at all
		t.Fatal(err)
	}

	out, errw, code := run(t, "find", "--json")
	if code != ExitOK {
		t.Fatalf("find exit %d from outside a vault with one saved: %s", code, errw)
	}
	if decode(t, out)["command"] != "find" {
		t.Errorf("unexpected envelope: %s", out)
	}
}

// Standing inside a vault must still use that one: a saved default that
// overrode the working directory would be the surprising answer.
func TestTheWorkingDirectoryBeatsTheSavedVault(t *testing.T) {
	settingsIn(t)
	saved := initVault(t, "saved")
	run(t, "use", saved)
	here := initVault(t, "here")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Dir(here)); err != nil {
		t.Fatal(err)
	}

	out, errw, code := run(t, "doctor", "--json")
	if code != ExitOK {
		t.Fatalf("doctor exit %d: %s", code, errw)
	}
	if !strings.Contains(out, filepath.Dir(here)) {
		t.Errorf("doctor did not use the vault the working directory is inside: %s", out)
	}
	if strings.Contains(out, filepath.Dir(saved)) {
		t.Errorf("the saved vault overrode the working directory: %s", out)
	}
}

// An explicit --vault naming nothing is an error, never a fall-through to the
// saved one: the user named a vault, and quietly using a different one is how
// the wrong documents get moved.
func TestAnExplicitVaultNeverFallsBackToTheSavedOne(t *testing.T) {
	settingsIn(t)
	saved := initVault(t, "saved")
	run(t, "use", saved)

	missing := filepath.Join(t.TempDir(), "nowhere")
	out, errw, code := run(t, "--vault", missing, "find")
	if code == ExitOK {
		t.Fatalf("an explicit --vault that does not exist succeeded: %s", out)
	}
	if strings.Contains(out, filepath.Dir(saved)) || strings.Contains(errw, filepath.Dir(saved)) {
		t.Errorf("fell back to the saved vault: %s %s", out, errw)
	}
}

// Likewise KAGAZ_VAULT: it is the user naming a vault, same as the flag.
func TestKagazVaultEnvNeverFallsBackToTheSavedOne(t *testing.T) {
	settingsIn(t)
	saved := initVault(t, "saved")
	run(t, "use", saved)

	t.Setenv(VaultEnv, filepath.Join(t.TempDir(), "nowhere"))
	_, errw, code := run(t, "find")
	if code == ExitOK {
		t.Fatal("KAGAZ_VAULT naming nothing fell through to the saved vault")
	}
	if strings.Contains(errw, filepath.Dir(saved)) {
		t.Errorf("the error named the saved vault: %s", errw)
	}
}

// A saved vault that has since been deleted needs a different repair from
// never having chosen one, so it gets a different message.
func TestAMissingSavedVaultSaysSoRatherThanSuggestingInit(t *testing.T) {
	settingsIn(t)
	vault := initVault(t, "deleted")
	run(t, "use", vault)
	if err := os.RemoveAll(filepath.Dir(vault)); err != nil {
		t.Fatal(err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	_, errw, code := run(t, "find")
	if code == ExitOK {
		t.Fatal("find succeeded with a saved vault that is gone")
	}
	if !strings.Contains(errw, "no longer there") {
		t.Errorf("the error does not say the saved vault is gone: %s", errw)
	}
	if !strings.Contains(errw, "kagaz use") {
		t.Errorf("the error does not offer the repair: %s", errw)
	}
}
