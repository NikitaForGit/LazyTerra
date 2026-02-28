// Package identity extracts execution context (environment, profile, region)
// from terragrunt HCL configuration files. It reads the module's terragrunt.hcl
// and its root include to derive what Terragrunt itself will use at runtime.
package identity

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// EnvLevel classifies the environment risk level for color coding.
type EnvLevel int

const (
	EnvNeutral EnvLevel = iota // dev, sandbox, default
	EnvStaging                 // staging, stg, preprod
	EnvProd                    // prod, production, live
)

// HeaderContext holds everything needed to render the header bar.
type HeaderContext struct {
	Env      string   // environment name or relative path fallback
	EnvLevel EnvLevel // for color coding
	Profile  string   // AWS profile name
	Region   string   // AWS region
}

// ---------------------------------------------------------------------------
// Main entry point: resolve header context for a module
// ---------------------------------------------------------------------------

// ResolveContext reads the module's terragrunt.hcl and its root include
// to derive environment, profile, and region.
//
// moduleAbsDir: absolute path to the module directory
// moduleRelDir: relative path from the terragrunt root
// hclContent:   contents of the module's terragrunt.hcl (already read by the caller)
func ResolveContext(moduleAbsDir, moduleRelDir, hclContent string) HeaderContext {
	ctx := HeaderContext{}

	// 1. Find and read the root config (via include path).
	rootContent, rootPath := resolveRootContent(hclContent, moduleAbsDir)

	// Derive account_name from the root config's parent directory.
	// This matches: account_name = basename(dirname(find_in_parent_folders("root.hcl")))
	accountName := ""
	if rootPath != "" {
		accountName = filepath.Base(filepath.Dir(rootPath))
	}

	// 2. Extract profile from root config (or module HCL as fallback).
	ctx.Profile = extractProfile(rootContent, accountName)
	if ctx.Profile == "" {
		ctx.Profile = extractProfile(hclContent, accountName)
	}

	// 3. Extract region: prefer path (module-specific), fall back to root config literal.
	ctx.Region = regionFromPath(moduleRelDir)
	if ctx.Region == "" {
		ctx.Region = extractRegion(rootContent)
	}

	// 4. Extract environment: prefer root locals.environment, then path heuristic.
	ctx.Env, ctx.EnvLevel = resolveEnv(rootContent, moduleRelDir)

	return ctx
}

// ---------------------------------------------------------------------------
// Root config resolution
// ---------------------------------------------------------------------------

// FindRootHCL locates the root HCL file for the module at moduleAbsDir by
// reading the module's terragrunt.hcl, parsing the include path, and walking
// up the directory tree. Returns the absolute path to root.hcl, or empty string.
func FindRootHCL(moduleAbsDir string) string {
	hclPath := filepath.Join(moduleAbsDir, "terragrunt.hcl")
	data, err := os.ReadFile(hclPath)
	if err != nil {
		return ""
	}
	hclContent := string(data)

	filename := ""
	if m := reIncludePath.FindStringSubmatch(hclContent); len(m) >= 2 {
		filename = m[1]
	} else if reIncludeNoArg.MatchString(hclContent) {
		filename = "terragrunt.hcl"
	}
	if filename == "" {
		return ""
	}
	return findInParentFolders(moduleAbsDir, filename)
}

var (
	// Match: include "<name>" { path = find_in_parent_folders("<filename>") }
	reIncludePath = regexp.MustCompile(`include\s+"[^"]*"\s*\{[^}]*path\s*=\s*find_in_parent_folders\(\s*"([^"]+)"\s*\)`)
	// Fallback: find_in_parent_folders() with no args
	reIncludeNoArg = regexp.MustCompile(`include\s+"[^"]*"\s*\{[^}]*path\s*=\s*find_in_parent_folders\(\s*\)`)
)

// resolveRootContent finds the root HCL file referenced by the module's include
// and returns its contents and absolute path. Returns ("", "") if not found.
func resolveRootContent(hclContent, moduleAbsDir string) (string, string) {
	// Try to find the include filename.
	filename := ""
	if m := reIncludePath.FindStringSubmatch(hclContent); len(m) >= 2 {
		filename = m[1]
	} else if reIncludeNoArg.MatchString(hclContent) {
		filename = "terragrunt.hcl"
	}

	if filename == "" {
		return "", ""
	}

	// Walk up from module dir looking for the file (mimics find_in_parent_folders).
	rootPath := findInParentFolders(moduleAbsDir, filename)
	if rootPath == "" {
		return "", ""
	}

	data, err := os.ReadFile(rootPath)
	if err != nil {
		return "", ""
	}
	return string(data), rootPath
}

// findInParentFolders walks up from dir looking for a file with the given name.
// Returns the absolute path if found, empty string otherwise.
func findInParentFolders(dir, filename string) string {
	// Start from the parent of the module dir (the module itself has terragrunt.hcl,
	// but find_in_parent_folders looks in ancestors).
	current := filepath.Dir(dir)
	for {
		candidate := filepath.Join(current, filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached filesystem root.
			break
		}
		current = parent
	}
	return ""
}

// ---------------------------------------------------------------------------
// Field extraction from root HCL content
// ---------------------------------------------------------------------------

var (
	// Matches profile with optional ternary: local.is_ci ? null : "name"
	reProfile = regexp.MustCompile(`profile\s*=\s*(?:local\.is_ci\s*\?\s*null\s*:\s*)?"([^"]+)"`)
	// Simple form: profile = "..."
	reProfileSimple = regexp.MustCompile(`profile\s*=\s*"([^"]+)"`)

	// Literal region assignment
	reRegionLiteral = regexp.MustCompile(`(?:aws_region|region)\s*=\s*"([a-z]{2}-[a-z]+-\d+)"`)

	// Literal environment assignment
	reEnvLiteral = regexp.MustCompile(`environment\s*=\s*"([^"]+)"`)

	// AWS region pattern for path detection
	reAWSRegion = regexp.MustCompile(`\b(us|eu|ap|sa|ca|me|af)-(east|west|south|north|central|southeast|northeast|southwest|northwest)-\d\b`)

	// Env classification — exact segment match
	prodPatterns    = regexp.MustCompile(`(?i)^(prod|production|prd|live)$`)
	stagingPatterns = regexp.MustCompile(`(?i)^(staging|stg|stage|preprod|pre-prod|uat)$`)
	neutralPatterns = regexp.MustCompile(`(?i)^(dev|development|sandbox|test|qa)$`)

	// Env classification — suffix/prefix match (hyphen-separated)
	prodSuffixPrefix    = regexp.MustCompile(`(?i)(?:^|.*-)(prod|production|prd|live)(?:-.*|$)`)
	stagingSuffixPrefix = regexp.MustCompile(`(?i)(?:^|.*-)(staging|stg|stage|preprod|uat)(?:-.*|$)`)
	neutralSuffixPrefix = regexp.MustCompile(`(?i)(?:^|.*-)(dev|development|sandbox|test|qa)(?:-.*|$)`)
)

// extractProfile finds the AWS profile from HCL content.
// accountName is used to resolve ${local.account_name} interpolations.
func extractProfile(content, accountName string) string {
	if content == "" {
		return ""
	}
	// Try the conditional form first: local.is_ci ? null : "profile-name"
	if m := reProfile.FindStringSubmatch(content); len(m) >= 2 {
		return resolveProfileValue(m[1], accountName)
	}
	// Fallback to simple form
	if m := reProfileSimple.FindStringSubmatch(content); len(m) >= 2 {
		return resolveProfileValue(m[1], accountName)
	}
	return ""
}

// resolveProfileValue resolves a profile string, substituting known interpolations.
func resolveProfileValue(val, accountName string) string {
	if !strings.Contains(val, "${") {
		return val
	}
	// Resolve ${local.account_name} if we know the account name.
	if accountName != "" && strings.Contains(val, "${local.account_name}") {
		resolved := strings.ReplaceAll(val, "${local.account_name}", accountName)
		// If there are still unresolved interpolations, give up.
		if !strings.Contains(resolved, "${") {
			return resolved
		}
	}
	return ""
}

// extractRegion finds a literal region from HCL content.
func extractRegion(content string) string {
	if content == "" {
		return ""
	}
	if m := reRegionLiteral.FindStringSubmatch(content); len(m) >= 2 {
		return m[1]
	}
	return ""
}

// regionFromPath extracts an AWS region from the module's relative path.
func regionFromPath(relDir string) string {
	segments := strings.Split(relDir, "/")
	for _, seg := range segments {
		if reAWSRegion.MatchString(seg) {
			return seg
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Environment resolution
// ---------------------------------------------------------------------------

// resolveEnv determines the environment name and level.
// Priority:
//  1. locals.environment literal in root config (e.g. environment = "prod")
//  2. Exact path segment match (e.g. .../prod/... → prod)
//  3. Suffix/prefix match in path segments (e.g. vpc-prod → prod, dev-tools → dev)
//  4. Fallback: relative path as env label
func resolveEnv(rootContent, moduleRelDir string) (string, EnvLevel) {
	// 1. Try literal environment from root config.
	if rootContent != "" {
		if m := reEnvLiteral.FindStringSubmatch(rootContent); len(m) >= 2 {
			env := m[1]
			return env, classifyEnv(env)
		}
	}

	segments := strings.Split(moduleRelDir, "/")

	// 2. Exact segment match — highest confidence.
	for _, seg := range segments {
		lower := strings.ToLower(seg)
		if prodPatterns.MatchString(lower) {
			return lower, EnvProd
		}
		if stagingPatterns.MatchString(lower) {
			return lower, EnvStaging
		}
		if neutralPatterns.MatchString(lower) {
			return lower, EnvNeutral
		}
	}

	// 3. Suffix/prefix match — check for env keywords separated by hyphens.
	for _, seg := range segments {
		lower := strings.ToLower(seg)
		if m := prodSuffixPrefix.FindStringSubmatch(lower); len(m) >= 2 {
			return m[1], EnvProd
		}
		if m := stagingSuffixPrefix.FindStringSubmatch(lower); len(m) >= 2 {
			return m[1], EnvStaging
		}
		if m := neutralSuffixPrefix.FindStringSubmatch(lower); len(m) >= 2 {
			return m[1], EnvNeutral
		}
	}

	// 4. Fallback: build a label from the path (strip the last component).
	if len(segments) > 1 {
		return strings.Join(segments[:len(segments)-1], "/"), EnvNeutral
	}

	return "", EnvNeutral
}

// classifyEnv determines the risk level from an environment name string.
func classifyEnv(env string) EnvLevel {
	lower := strings.ToLower(env)
	if prodPatterns.MatchString(lower) {
		return EnvProd
	}
	if stagingPatterns.MatchString(lower) {
		return EnvStaging
	}
	return EnvNeutral
}
