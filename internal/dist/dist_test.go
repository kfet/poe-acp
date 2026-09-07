package dist

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kfet/distkit/installsh"
)

// repoRoot is the directory holding install.sh / install.sh.json.
const repoRoot = "../.."

func TestConfigNamesTheAssetsGoreleaserPublishes(t *testing.T) {
	cfg := Config("v9.9.9")
	if cfg.Repo != "kfet/poe-acp" || cfg.Binary != "poe-acp" || cfg.AssetStem != "poe-acp" {
		t.Fatalf("unexpected identity: %+v", cfg)
	}
	if cfg.Version != "v9.9.9" {
		t.Fatalf("Version = %q, want the string passed in", cfg.Version)
	}
	if cfg.RestartHint == "" {
		t.Fatal("RestartHint must be set: a swapped binary is inert until recycled")
	}
	// .goreleaser.yaml publishes raw binaries named
	// poe-acp-{{.Os}}-{{.Arch}}{{if .Arm}}v{{.Arm}}{{end}} — i.e. armv6
	// for the GOARM=6 build. A mismatch here is a 404 on every fleet host.
	if got, want := cfg.AssetName("v9.9.9"), "poe-acp-"+runtime.GOOS+"-"+assetArch(); got != want {
		t.Fatalf("AssetName = %q, want %q", got, want)
	}
}

func assetArch() string {
	if runtime.GOARCH == "arm" {
		return "armv6"
	}
	return runtime.GOARCH
}

// The install.sh spec must not disagree with the config the binary
// self-updates with: if they name different assets, `install.sh` and
// `poe-acp update` fetch different files from the same release.
func TestInstallShSpecAgreesWithConfig(t *testing.T) {
	spec, err := installsh.LoadSpec(filepath.Join(repoRoot, "install.sh.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := installsh.FromConfig(Config("v9.9.9"))
	if spec.Repo != want.Repo {
		t.Errorf("repo = %q, want %q", spec.Repo, want.Repo)
	}
	if spec.Binary != want.Binary {
		t.Errorf("binary = %q, want %q", spec.Binary, want.Binary)
	}
	if spec.AssetStem != want.AssetStem {
		t.Errorf("asset_stem = %q, want %q", spec.AssetStem, want.AssetStem)
	}
	if spec.AssetTemplate != want.AssetTemplate {
		t.Errorf("asset_template = %q, want %q", spec.AssetTemplate, want.AssetTemplate)
	}
	if spec.ArmSuffix != want.ArmSuffix {
		t.Errorf("arm_suffix = %q, want %q", spec.ArmSuffix, want.ArmSuffix)
	}
	if spec.NoChecksums != want.NoChecksums {
		t.Errorf("no_checksums = %v, want %v", spec.NoChecksums, want.NoChecksums)
	}
	if spec.ChecksumsAsset != want.ChecksumsAsset {
		t.Errorf("checksums_asset = %q, want %q", spec.ChecksumsAsset, want.ChecksumsAsset)
	}
}

// The checked-in install.sh is generated, never hand-edited. `make
// check-installsh` enforces this too; the test catches it without a
// separate target being run.
func TestInstallShIsNotDrifted(t *testing.T) {
	spec, err := installsh.LoadSpec(filepath.Join(repoRoot, "install.sh.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := installsh.CheckDrift(filepath.Join(repoRoot, "install.sh"), spec); err != nil {
		t.Fatalf("%v\nregenerate with: make install.sh", err)
	}
}

// curl … | sh only resolves a script at the repo root, and it must be
// executable for a `git clone && ./install.sh` install.
func TestInstallShIsExecutableAtTheRoot(t *testing.T) {
	fi, err := os.Stat(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("install.sh mode %v is not executable", fi.Mode().Perm())
	}
}
