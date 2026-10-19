package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The runner passes an action's inputs as INPUT_<NAME>, hyphens kept; the
// wrapper must hand them to node unchanged.
func TestNodeWrapperKeepsHyphenatedInputs(t *testing.T) {
	dir := t.TempDir()
	wrapper, err := Context.ReadFile("node.boxrunner")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node"), wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	// node.real stands in for node and prints what it was given.
	if err := os.WriteFile(filepath.Join(dir, "node.real"), []byte("#!/usr/bin/env bash\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(dir, "node"))
	cmd.Env = append(os.Environ(), "INPUT_ANALYSIS-KINDS=code-scanning", "INPUT_LANGUAGES=actions")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"INPUT_ANALYSIS-KINDS=code-scanning", "INPUT_LANGUAGES=actions"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("node did not get %s", want)
		}
	}
}
