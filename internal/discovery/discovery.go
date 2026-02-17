package discovery

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const targetFile = "terragrunt.hcl"

// Module represents a discovered terragrunt module.
type Module struct {
	// Dir is the directory containing terragrunt.hcl, relative to the scan root.
	Dir string
	// AbsDir is the absolute path to the directory.
	AbsDir string
}

// Scan walks the directory tree starting at root and returns all directories
// that contain a terragrunt.hcl file. Paths are returned relative to root.
// Hidden directories (starting with '.') and .terragrunt-cache are skipped.
func Scan(root string) ([]Module, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	var modules []Module

	err = filepath.Walk(absRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		// Skip hidden directories and terragrunt cache.
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			if name == ".terragrunt-cache" {
				return filepath.SkipDir
			}
			if name == ".terraform" {
				return filepath.SkipDir
			}
			return nil
		}

		if info.Name() == targetFile {
			dir := filepath.Dir(path)
			rel, err := filepath.Rel(absRoot, dir)
			if err != nil {
				rel = dir
			}
			if rel == "." {
				rel = "."
			}
			modules = append(modules, Module{
				Dir:    rel,
				AbsDir: dir,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(modules, func(i, j int) bool {
		return modules[i].Dir < modules[j].Dir
	})

	return modules, nil
}
