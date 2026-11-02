package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// update is `brew update && brew upgrade tailreach && tailreach setup` in one go: forgetting
// the setup step would leave the old copy running. A checkout is updated with git and make.
func update(args []string) error {
	exe := selfPath()
	if !managedCopy(exe) {
		return fmt.Errorf("this copy was built from a clone: update it with `git pull && make setup`")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		return fmt.Errorf("Homebrew isn't on your PATH: run `brew upgrade tailreach && tailreach setup` yourself")
	}
	for _, step := range [][]string{{"update"}, {"upgrade", "tailreach"}} {
		fmt.Println("→ brew", strings.Join(step, " "))
		cmd := exec.Command(brew, step...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("brew %s: %v", strings.Join(step, " "), err)
		}
	}
	// Setup from the new version (Homebrew's bin link now points at it), not this old one.
	out, err := exec.Command(brew, "--prefix").Output()
	if err != nil {
		return err
	}
	next := filepath.Join(strings.TrimSpace(string(out)), "bin", "tailreach")
	fmt.Println("→ tailreach setup")
	cmd := exec.Command(next, append([]string{"setup"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
