# CI Workflow Documentation

## What was added

1. **`.github/workflows/ci.yml`** — Continuous Integration workflow
2. **`.golangci.yml`** — Linter configuration
3. **Updated `Makefile`** — Added race detector to tests, improved lint target

## What the CI does

The CI runs automatically on:
- Every push to `main`
- Every pull request to `main`

### Jobs

**1. Test Job** (6 combinations)
- Runs on: Ubuntu + macOS
- Go versions: 1.22, 1.23, 1.24
- Steps:
  - Verify go.mod/go.sum integrity
  - Build all packages
  - Run tests with race detector (`-race` flag catches concurrency bugs)

**2. Lint Job**
- Runs golangci-lint (11 linters enabled)
- Catches common issues: unchecked errors, unused code, formatting, etc.

**3. Go Mod Tidy Job**
- Ensures go.mod/go.sum are clean
- Prevents "works on my machine" due to missing dependencies

## Running CI checks locally

Before pushing code, run the same checks CI will run:

```bash
# Run tests (with race detector, like CI)
make test

# Run linter (requires golangci-lint installed)
make lint

# Install golangci-lint (if needed)
brew install golangci-lint  # macOS
# OR
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Verify dependencies
go mod verify
go mod tidy
```

## What happens when CI fails

If you push code and CI fails:
1. GitHub will mark the commit with a red ❌
2. If it's a PR, the checks will show as failed
3. Click "Details" next to the failed job to see logs
4. Fix the issue locally, commit, and push again

## Linters enabled

- **errcheck** — Unchecked errors
- **gosimple** — Simplification suggestions
- **govet** — Standard Go vet
- **ineffassign** — Ineffectual assignments
- **staticcheck** — Static analysis
- **unused** — Unused code
- **gofmt** — Code formatting
- **goimports** — Import formatting
- **misspell** — Spelling in comments/strings
- **revive** — General linting
- **typecheck** — Type correctness

## Why these Go versions?

- **1.22** — Feb 2024 release, still widely used in enterprise
- **1.23** — Aug 2024 release, common in active projects  
- **1.24** — Feb 2025 release, latest stable (your current version)

This covers ~90% of active Go users.
