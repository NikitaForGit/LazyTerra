package deps

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	content := `
include "root" {
  path = find_in_parent_folders()
}

dependency "vpc" {
  config_path = "../vpc"
}

dependency "rds" {
  config_path = "../rds"
}

dependencies {
  paths = ["../iam", "../s3"]
}
`
	tmp := t.TempDir()
	hclPath := filepath.Join(tmp, "terragrunt.hcl")
	if err := os.WriteFile(hclPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	deps, err := Parse(hclPath)
	if err != nil {
		t.Fatal(err)
	}

	// Expected: 1 include + 2 dependency + 2 dependencies = 5.
	if len(deps) != 5 {
		t.Fatalf("expected 5 dependencies, got %d: %+v", len(deps), deps)
	}

	// Check include.
	if deps[0].Type != "include" || deps[0].Label != "root" {
		t.Errorf("deps[0]: expected include/root, got %s/%s", deps[0].Type, deps[0].Label)
	}

	// Check dependency "vpc".
	if deps[1].Type != "dependency" || deps[1].Label != "vpc" || deps[1].ConfigPath != "../vpc" {
		t.Errorf("deps[1]: unexpected %+v", deps[1])
	}

	// Check dependencies paths.
	if deps[3].Type != "dependencies" || deps[3].ConfigPath != "../iam" {
		t.Errorf("deps[3]: unexpected %+v", deps[3])
	}
	if deps[4].Type != "dependencies" || deps[4].ConfigPath != "../s3" {
		t.Errorf("deps[4]: unexpected %+v", deps[4])
	}
}

func TestParseEmpty(t *testing.T) {
	tmp := t.TempDir()
	hclPath := filepath.Join(tmp, "terragrunt.hcl")
	if err := os.WriteFile(hclPath, []byte("# nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deps, err := Parse(hclPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Fatalf("expected 0 dependencies, got %d", len(deps))
	}
}
