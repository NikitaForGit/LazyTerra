# Contributing to LazyTerra

Thank you for your interest in contributing to LazyTerra! This document provides guidelines and instructions for contributing.

## Getting Started

### Prerequisites

- **Go 1.22 or higher** (the project uses Go 1.24.2 for development)
- **Make** (for build automation)
- **Terragrunt and OpenTofu** (for testing)
- **golangci-lint** (for linting)

### Setting Up Your Development Environment

1. Fork the repository on GitHub
2. Clone your fork:
   ```bash
   git clone https://github.com/YOUR_USERNAME/LazyTerra.git
   cd LazyTerra
   ```

3. Install dependencies:
   ```bash
   go mod download
   ```

4. Install golangci-lint:
   ```bash
   # macOS
   brew install golangci-lint
   
   # Linux
   curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin
   ```

5. Build the project:
   ```bash
   make build
   ```

## Development Workflow

### Building

```bash
# Build the binary
make build

# Install to $GOPATH/bin
make install
```

### Testing

```bash
# Run tests with race detector
make test

# Run tests in a specific package
go test -v ./internal/deps
```

### Linting

```bash
# Run all linters
make lint
```

We use golangci-lint with the following linters enabled:
- errcheck
- govet
- ineffassign
- staticcheck (includes gosimple)
- unused
- misspell
- gofmt
- goimports

### Code Style

- Follow standard Go conventions
- Run `gofmt` and `goimports` before committing (the linter will check this)
- Keep functions focused and reasonably sized
- Write tests for new functionality when possible

## Project Structure

```
lazyterra/
├── cmd/lazyterra/     # Main application entry point
├── internal/
│   ├── deps/          # Dependency parsing
│   ├── discovery/     # Module discovery
│   ├── identity/      # AWS/environment context detection
│   ├── runner/        # Command execution
│   ├── ui/            # Bubble Tea TUI
│   └── version/       # OpenTofu/Terragrunt version detection
├── .github/
│   └── workflows/     # CI/CD configuration
└── Makefile           # Build automation
```

## Making Changes

### Creating a Branch

Create a feature branch from `main`:

```bash
git checkout -b feature/your-feature-name
```

Use descriptive branch names:
- `feature/` - New features
- `fix/` - Bug fixes
- `docs/` - Documentation changes
- `refactor/` - Code refactoring

### Commit Messages

Write clear, concise commit messages:

```
Add force-unlock command with lock ID prompt

- Implement 'U' keybinding in modules pane
- Add text input overlay for lock ID entry
- Create CmdForceUnlock factory function in runner
```

### Before Submitting

1. **Run tests**: `make test`
2. **Run linter**: `make lint`
3. **Build the project**: `make build`
4. **Test your changes manually** with a real Terragrunt project

## Submitting a Pull Request

1. Push your changes to your fork:
   ```bash
   git push origin feature/your-feature-name
   ```

2. Open a pull request on GitHub

3. Provide a clear description of:
   - What the PR does
   - Why the change is needed
   - Any related issues (use "Fixes #123" to auto-close issues)
   - Screenshots/GIFs if the change affects the UI

4. Wait for CI checks to pass

5. Respond to review feedback

## Pull Request Guidelines

- Keep PRs focused on a single feature or fix
- Update documentation if needed
- Add tests for new functionality when possible
- Ensure CI passes (tests and linting)
- Be responsive to feedback

## Continuous Integration

Our CI runs on every push and pull request:

- **Test**: Runs tests on Ubuntu and macOS with Go 1.22, 1.23, and 1.24
- **Lint**: Runs golangci-lint to check code quality
- **Mod Tidy**: Verifies go.mod and go.sum are clean

See [.github/CI.md](.github/CI.md) for more details.

## Feature Requests and Bug Reports

- Use GitHub Issues to report bugs or request features
- Search existing issues before creating a new one
- Provide as much detail as possible:
  - For bugs: steps to reproduce, expected vs actual behavior, environment details
  - For features: use case, proposed solution, alternatives considered

## Platform Support

LazyTerra currently supports:
- **Linux** ✅ (tested)
- **macOS** ✅ (tested)
- **Windows** ❌ (not yet supported)

If you're interested in adding Windows support, please open an issue to discuss the approach first.

## Need Help?

- Open an issue for questions or discussions
- Check existing issues and pull requests
- Review the README.md for usage instructions

## License

By contributing to LazyTerra, you agree that your contributions will be licensed under the MIT License.
