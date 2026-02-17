package git

import (
	"os/exec"
	"strings"
)

// Branch represents a git branch.
type Branch struct {
	Name      string
	IsCurrent bool
	IsRemote  bool
}

// CurrentBranch returns the name of the currently checked-out branch.
func CurrentBranch() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ListBranches returns all local branches, marking the current one.
func ListBranches() ([]Branch, error) {
	out, err := exec.Command("git", "branch", "--format=%(refname:short)\t%(HEAD)").Output()
	if err != nil {
		return nil, err
	}

	var branches []Branch
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		name := parts[0]
		isCurrent := len(parts) > 1 && strings.TrimSpace(parts[1]) == "*"
		branches = append(branches, Branch{
			Name:      name,
			IsCurrent: isCurrent,
		})
	}
	return branches, nil
}

// ListRemoteBranches returns remote tracking branches.
func ListRemoteBranches() ([]Branch, error) {
	out, err := exec.Command("git", "branch", "-r", "--format=%(refname:short)").Output()
	if err != nil {
		return nil, err
	}

	var branches []Branch
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "HEAD") {
			continue
		}
		branches = append(branches, Branch{
			Name:     line,
			IsRemote: true,
		})
	}
	return branches, nil
}

// Checkout switches to the given branch.
func Checkout(branch string) error {
	return exec.Command("git", "checkout", branch).Run()
}

// IsGitRepo returns true if the current directory is inside a git repository.
func IsGitRepo() bool {
	err := exec.Command("git", "rev-parse", "--git-dir").Run()
	return err == nil
}
