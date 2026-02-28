package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/NikitaForGit/LazyTerra/internal/discovery"
	"github.com/NikitaForGit/LazyTerra/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
)

// version is set at build time via -ldflags "-X main.version=vX.Y.Z"
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("lazyterra %s\n", version)
		os.Exit(0)
	}

	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving path: %v\n", err)
		os.Exit(1)
	}

	modules, err := discovery.Scan(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning for terragrunt modules: %v\n", err)
		os.Exit(1)
	}

	if len(modules) == 0 {
		fmt.Fprintln(os.Stderr, "No terragrunt.hcl files found in the current directory tree.")
		os.Exit(0)
	}

	model := ui.NewModel(modules, absRoot, version)
	p := tea.NewProgram(model, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
