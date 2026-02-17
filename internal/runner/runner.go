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
)

// OutputLine represents a single line of command output.
type OutputLine struct {
	Module    string
	Text      string
	IsError   bool
	Timestamp time.Time
}

// Result is sent when a command completes for a module.
type Result struct {
	Module  string
	Status  Status
	Elapsed time.Duration
}

// Runner manages the execution of terragrunt commands.
type Runner struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	running  bool
	statuses map[string]Status
	outputCh chan OutputLine
	resultCh chan Result
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
		r.outputCh <- OutputLine{
			Text:      header,
			Timestamp: time.Now(),
		}

		for _, modDir := range moduleDirs {
			if ctx.Err() != nil {
				r.outputCh <- OutputLine{
					Text:      "⚠ Cancelled",
					Timestamp: time.Now(),
				}
				break
			}

			absDir := absDirs[modDir]
			r.mu.Lock()
			r.statuses[modDir] = StatusRunning
			r.mu.Unlock()

			r.outputCh <- OutputLine{
				Module:    modDir,
				Text:      fmt.Sprintf("▶ [%s] terragrunt %s", modDir, cmd.Name),
				Timestamp: time.Now(),
			}

			start := time.Now()
			err := r.execCommand(ctx, cmd, modDir, absDir)
			elapsed := time.Since(start)

			status := StatusSuccess
			if err != nil {
				status = StatusError
				r.outputCh <- OutputLine{
					Module:    modDir,
					Text:      fmt.Sprintf("✗ [%s] failed: %v (%.1fs)", modDir, err, elapsed.Seconds()),
					IsError:   true,
					Timestamp: time.Now(),
				}
			} else {
				r.outputCh <- OutputLine{
					Module:    modDir,
					Text:      fmt.Sprintf("✓ [%s] completed (%.1fs)", modDir, elapsed.Seconds()),
					Timestamp: time.Now(),
				}
			}

			r.mu.Lock()
			r.statuses[modDir] = status
			r.mu.Unlock()

			r.resultCh <- Result{
				Module:  modDir,
				Status:  status,
				Elapsed: elapsed,
			}
		}

		r.outputCh <- OutputLine{
			Text:      "━━━ Done ━━━",
			Timestamp: time.Now(),
		}
	}()
}

func (r *Runner) execCommand(ctx context.Context, cmd Command, modDir, absDir string) error {
	args := make([]string, len(cmd.Args))
	copy(args, cmd.Args)

	c := exec.CommandContext(ctx, "terragrunt", args...)
	c.Dir = absDir
	c.Env = append(c.Environ(), "TF_IN_AUTOMATION=1")

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
			Module:    modDir,
			Text:      fmt.Sprintf("  [%s] %s", modDir, line),
			IsError:   isErr,
			Timestamp: time.Now(),
		}
	}

	return c.Wait()
}
