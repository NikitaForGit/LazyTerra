package version

import (
	"os/exec"
	"regexp"
	"strings"
)

// Info holds version information for terraform and terragrunt.
type Info struct {
	Terraform  string
	Terragrunt string
}

var versionRe = regexp.MustCompile(`v?(\d+\.\d+\.\d+\S*)`)

// Detect runs terraform --version and terragrunt --version and extracts
// the version strings. Returns "not found" if a binary is missing.
func Detect() Info {
	return Info{
		Terraform:  detectOne("terraform"),
		Terragrunt: detectOne("terragrunt"),
	}
}

func detectOne(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "not found"
	}
	// Take only the first line.
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if m := versionRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return strings.TrimSpace(line)
}
