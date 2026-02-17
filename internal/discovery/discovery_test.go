package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScan(t *testing.T) {
	// Create a temp directory structure.
	tmp := t.TempDir()

	// Create module directories.
	dirs := []string{
		"env/dev/vpc",
		"env/dev/rds",
		"env/prod/vpc",
		".hidden/secret",
		"env/dev/vpc/.terragrunt-cache/somecache",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Create terragrunt.hcl files.
	hclFiles := []string{
		"env/dev/vpc/terragrunt.hcl",
		"env/dev/rds/terragrunt.hcl",
		"env/prod/vpc/terragrunt.hcl",
		".hidden/secret/terragrunt.hcl",                          // should be skipped
		"env/dev/vpc/.terragrunt-cache/somecache/terragrunt.hcl", // should be skipped
	}
	for _, f := range hclFiles {
		path := filepath.Join(tmp, f)
		if err := os.WriteFile(path, []byte("# test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	modules, err := Scan(tmp)
	if err != nil {
		t.Fatal(err)
	}

	// Should find 3 modules (hidden and cache dirs skipped).
	if len(modules) != 3 {
		t.Fatalf("expected 3 modules, got %d: %+v", len(modules), modules)
	}

	expected := []string{
		"env/dev/rds",
		"env/dev/vpc",
		"env/prod/vpc",
	}
	for i, exp := range expected {
		if modules[i].Dir != exp {
			t.Errorf("module[%d]: expected %q, got %q", i, exp, modules[i].Dir)
		}
	}
}

func TestScanEmpty(t *testing.T) {
	tmp := t.TempDir()

	modules, err := Scan(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 0 {
		t.Fatalf("expected 0 modules, got %d", len(modules))
	}
}
