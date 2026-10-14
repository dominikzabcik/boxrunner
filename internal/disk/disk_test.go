package disk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsed(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "overlay2", "l"), 0o755)
	os.WriteFile(filepath.Join(dir, "overlay2", "layer.tar"), make([]byte, 3000), 0o644)
	os.WriteFile(filepath.Join(dir, "engine-id"), make([]byte, 96), 0o644)
	os.Symlink(filepath.Join(dir, "overlay2", "layer.tar"), filepath.Join(dir, "overlay2", "l", "link"))
	if got := Used(dir); got != 3096 {
		t.Errorf("Used = %d, want 3096", got)
	}
	if got := Used(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("Used of a missing directory = %d", got)
	}
}

func TestLowestNamesTheUnreadablePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, path, err := Lowest(t.TempDir(), missing); err == nil || path != missing || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("Lowest = %q, %v", path, err)
	}
}
