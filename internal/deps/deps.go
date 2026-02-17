package deps

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Dependency represents a terragrunt dependency or include block.
type Dependency struct {
	// Label is the name given to the dependency block (e.g. "vpc").
	Label string
	// ConfigPath is the relative path referenced in the block.
	ConfigPath string
	// Type is "dependency", "dependencies", or "include".
	Type string
}

var (
	// dependency "vpc" {
	reDependency = regexp.MustCompile(`^\s*dependency\s+"([^"]+)"\s*\{`)
	// config_path = "../vpc"
	reConfigPath = regexp.MustCompile(`^\s*config_path\s*=\s*"([^"]+)"`)
	// paths = ["../vpc", "../rds"]
	rePaths = regexp.MustCompile(`^\s*paths\s*=\s*\[([^\]]*)\]`)
	// include "root" {
	reInclude = regexp.MustCompile(`^\s*include\s+"([^"]*)"?\s*\{`)
	// path = find_in_parent_folders() or path = "../../root.hcl"
	reIncludePath = regexp.MustCompile(`^\s*path\s*=\s*(?:find_in_parent_folders\(\)|"([^"]*)")`)
)

// Parse reads a terragrunt.hcl file and extracts dependency/include references.
func Parse(hclPath string) ([]Dependency, error) {
	f, err := os.Open(hclPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var deps []Dependency
	scanner := bufio.NewScanner(f)

	var currentLabel string
	var currentType string
	inBlock := false

	for scanner.Scan() {
		line := scanner.Text()

		// Match dependency "name" {
		if m := reDependency.FindStringSubmatch(line); m != nil {
			currentLabel = m[1]
			currentType = "dependency"
			inBlock = true
			continue
		}

		// Match include "name" {
		if m := reInclude.FindStringSubmatch(line); m != nil {
			currentLabel = m[1]
			if currentLabel == "" {
				currentLabel = "root"
			}
			currentType = "include"
			inBlock = true
			continue
		}

		if inBlock {
			// config_path inside a dependency block.
			if currentType == "dependency" {
				if m := reConfigPath.FindStringSubmatch(line); m != nil {
					deps = append(deps, Dependency{
						Label:      currentLabel,
						ConfigPath: m[1],
						Type:       currentType,
					})
					inBlock = false
					continue
				}
			}

			// path inside an include block.
			if currentType == "include" {
				if m := reIncludePath.FindStringSubmatch(line); m != nil {
					p := m[1]
					if p == "" {
						p = "find_in_parent_folders()"
					}
					deps = append(deps, Dependency{
						Label:      currentLabel,
						ConfigPath: p,
						Type:       currentType,
					})
					inBlock = false
					continue
				}
			}

			// Closing brace ends the block.
			if strings.TrimSpace(line) == "}" {
				inBlock = false
			}
		}

		// dependencies { paths = [...] }
		if m := rePaths.FindStringSubmatch(line); m != nil {
			paths := parsePaths(m[1])
			for _, p := range paths {
				label := filepath.Base(p)
				deps = append(deps, Dependency{
					Label:      label,
					ConfigPath: p,
					Type:       "dependencies",
				})
			}
		}
	}

	return deps, scanner.Err()
}

// parsePaths extracts quoted strings from a comma-separated list.
func parsePaths(s string) []string {
	var result []string
	re := regexp.MustCompile(`"([^"]+)"`)
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		result = append(result, m[1])
	}
	return result
}
