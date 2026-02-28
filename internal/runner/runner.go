package runner

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Status represents the execution state of a module.
type Status int

const (
	StatusIdle Status = iota
	StatusRunning
	StatusSuccess
	StatusError
)

// String returns a display string for the status.
func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "⟳"
	case StatusSuccess:
		return "✓"
	case StatusError:
		return "✗"
	default:
		return " "
	}
}

// Command represents a terragrunt command type.
type Command struct {
	Name string
	Args []string
}

var (
	CmdPlan            = Command{Name: "plan", Args: []string{"plan"}}
	CmdApply           = Command{Name: "apply", Args: []string{"apply", "-auto-approve"}}
	CmdValidate        = Command{Name: "validate", Args: []string{"validate"}}
	CmdInit            = Command{Name: "init", Args: []string{"init"}}
	CmdInitReconfigure = Command{Name: "init --reconfigure", Args: []string{"init", "--reconfigure"}}
	CmdDestroy         = Command{Name: "destroy", Args: []string{"destroy", "-auto-approve"}}
	CmdOutput          = Command{Name: "output", Args: []string{"output"}}
	CmdStateList       = Command{Name: "state list", Args: []string{"state", "list"}}
	CmdHclfmt          = Command{Name: "hclfmt", Args: []string{"hclfmt"}}
)

// CmdForceUnlock creates a force-unlock command with the given lock ID.
func CmdForceUnlock(lockID string) Command {
	return Command{Name: "force-unlock", Args: []string{"force-unlock", "-force", lockID}}
}

// OutputLine represents a single line of command output.
type OutputLine struct {
	Text    string
	IsError bool
}

// Result is sent when a command completes.
type Result struct{}

// Runner manages the execution of terragrunt commands.
type Runner struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	running  bool
	statuses map[string]Status
	outputCh chan OutputLine
	resultCh chan Result
	extraEnv []string // additional env vars (e.g. TF_LOG=DEBUG)
}

// SetExtraEnv sets additional environment variables that will be passed to
// all spawned terragrunt/terraform processes. Each entry should be "KEY=VALUE".
func (r *Runner) SetExtraEnv(env []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extraEnv = env
}

// New creates a new Runner.
func New() *Runner {
	return &Runner{
		statuses: make(map[string]Status),
		outputCh: make(chan OutputLine, 1000),
		resultCh: make(chan Result, 100),
	}
}

// OutputChan returns the channel for streaming output lines.
func (r *Runner) OutputChan() <-chan OutputLine {
	return r.outputCh
}

// ResultChan returns the channel for command results.
func (r *Runner) ResultChan() <-chan Result {
	return r.resultCh
}

// IsRunning returns true if commands are currently executing.
func (r *Runner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// GetStatus returns the current status for a module directory.
func (r *Runner) GetStatus(moduleDir string) Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statuses[moduleDir]
}

// ClearStatus removes the status for a module directory.
func (r *Runner) ClearStatus(moduleDir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.statuses, moduleDir)
}

// Cancel stops any running commands.
func (r *Runner) Cancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
}

// Run executes a terragrunt command against the given module directories sequentially.
func (r *Runner) Run(cmd Command, moduleDirs []string, absDirs map[string]string) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.running = false
			r.cancel = nil
			r.mu.Unlock()
			cancel()
		}()

		header := fmt.Sprintf("━━━ Running: terragrunt %s on %d module(s) ━━━", cmd.Name, len(moduleDirs))
		r.outputCh <- OutputLine{Text: header}

		for _, modDir := range moduleDirs {
			if ctx.Err() != nil {
				r.outputCh <- OutputLine{Text: "⚠ Cancelled"}
				break
			}

			absDir := absDirs[modDir]
			r.mu.Lock()
			r.statuses[modDir] = StatusRunning
			r.mu.Unlock()

			r.outputCh <- OutputLine{
				Text: fmt.Sprintf("▶ [%s] terragrunt %s", modDir, cmd.Name),
			}
			r.outputCh <- OutputLine{
				Text: fmt.Sprintf("  $ terragrunt %s", strings.Join(cmd.Args, " ")),
			}

			start := time.Now()
			err := r.execCommand(ctx, cmd, modDir, absDir)
			elapsed := time.Since(start)

			status := StatusSuccess
			if err != nil {
				status = StatusError
				r.outputCh <- OutputLine{
					Text:    fmt.Sprintf("✗ [%s] failed: %v (%.1fs)", modDir, err, elapsed.Seconds()),
					IsError: true,
				}
			} else {
				r.outputCh <- OutputLine{
					Text: fmt.Sprintf("✓ [%s] completed (%.1fs)", modDir, elapsed.Seconds()),
				}
			}

			r.mu.Lock()
			r.statuses[modDir] = status
			r.mu.Unlock()

			r.resultCh <- Result{}
		}

		r.outputCh <- OutputLine{Text: "━━━ Done ━━━"}
	}()
}

// RunAll executes a terragrunt run-all command with explicit include directories.
// This is more efficient than running commands sequentially when multiple modules are selected.
func (r *Runner) RunAll(cmd Command, moduleDirs []string, absDirs map[string]string, workDir string) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.running = false
			r.cancel = nil
			r.mu.Unlock()
			cancel()
		}()

		header := fmt.Sprintf("━━━ Running: terragrunt run-all %s on %d module(s) ━━━", cmd.Name, len(moduleDirs))
		r.outputCh <- OutputLine{Text: header}

		// Set all modules to running status
		r.mu.Lock()
		for _, modDir := range moduleDirs {
			r.statuses[modDir] = StatusRunning
		}
		r.mu.Unlock()

		r.outputCh <- OutputLine{
			Text: fmt.Sprintf("▶ terragrunt run-all %s", cmd.Name),
		}

		// Show the full command being executed
		fullArgs := []string{"run-all"}
		fullArgs = append(fullArgs, cmd.Args...)
		for _, modDir := range moduleDirs {
			absDir := absDirs[modDir]
			fullArgs = append(fullArgs, "--terragrunt-include-dir", absDir)
		}
		fullArgs = append(fullArgs, "--terragrunt-non-interactive")
		fullArgs = append(fullArgs, "--terragrunt-strict-include")
		fullArgs = append(fullArgs, "--terragrunt-ignore-external-dependencies")
		r.outputCh <- OutputLine{
			Text: fmt.Sprintf("  $ terragrunt %s", strings.Join(fullArgs, " ")),
		}

		start := time.Now()
		err := r.execRunAllCommand(ctx, cmd, moduleDirs, absDirs, workDir)
		elapsed := time.Since(start)

		// Determine final status for all modules
		status := StatusSuccess
		if err != nil {
			status = StatusError
			r.outputCh <- OutputLine{
				Text:    fmt.Sprintf("✗ run-all %s failed: %v (%.1fs)", cmd.Name, err, elapsed.Seconds()),
				IsError: true,
			}
		} else {
			r.outputCh <- OutputLine{
				Text: fmt.Sprintf("✓ run-all %s completed (%.1fs)", cmd.Name, elapsed.Seconds()),
			}
		}

		// Update all module statuses
		r.mu.Lock()
		for _, modDir := range moduleDirs {
			r.statuses[modDir] = status
		}
		r.mu.Unlock()

		// Send results for all modules
		for range moduleDirs {
			r.resultCh <- Result{}
		}

		r.outputCh <- OutputLine{Text: "━━━ Done ━━━"}
	}()
}

func (r *Runner) execRunAllCommand(ctx context.Context, cmd Command, moduleDirs []string, absDirs map[string]string, workDir string) error {
	// Build args: run-all <cmd args> --terragrunt-include-dir <dir1> --terragrunt-include-dir <dir2> ...
	args := []string{"run-all"}
	args = append(args, cmd.Args...)

	// Add include dirs for each selected module (using absolute paths)
	for _, modDir := range moduleDirs {
		absDir := absDirs[modDir]
		args = append(args, "--terragrunt-include-dir", absDir)
	}

	// Add non-interactive flag and strict include to prevent running dependencies
	args = append(args, "--terragrunt-non-interactive")
	args = append(args, "--terragrunt-strict-include")
	args = append(args, "--terragrunt-ignore-external-dependencies")

	c := exec.CommandContext(ctx, "terragrunt", args...)
	c.Dir = workDir
	env := append(c.Environ(), "TF_IN_AUTOMATION=1")
	r.mu.Lock()
	env = append(env, r.extraEnv...)
	r.mu.Unlock()
	c.Env = env

	// Merge stdout and stderr into a single pipe for display.
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	c.Stderr = c.Stdout // merge stderr into stdout pipe

	if err := c.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		isErr := strings.Contains(strings.ToLower(line), "error") ||
			strings.Contains(strings.ToLower(line), "failed")
		r.outputCh <- OutputLine{
			Text:    fmt.Sprintf("  %s", line),
			IsError: isErr,
		}
	}

	return c.Wait()
}

func (r *Runner) execCommand(ctx context.Context, cmd Command, _, absDir string) error {
	args := make([]string, len(cmd.Args))
	copy(args, cmd.Args)

	c := exec.CommandContext(ctx, "terragrunt", args...)
	c.Dir = absDir
	env := append(c.Environ(), "TF_IN_AUTOMATION=1")
	r.mu.Lock()
	env = append(env, r.extraEnv...)
	r.mu.Unlock()
	c.Env = env

	// Merge stdout and stderr into a single pipe for display.
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	c.Stderr = c.Stdout // merge stderr into stdout pipe

	if err := c.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		isErr := strings.Contains(strings.ToLower(line), "error") ||
			strings.Contains(strings.ToLower(line), "failed")
		r.outputCh <- OutputLine{
			Text:    fmt.Sprintf("  %s", line),
			IsError: isErr,
		}
	}

	return c.Wait()
}
