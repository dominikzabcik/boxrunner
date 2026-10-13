package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDebianLike(t *testing.T) {
	cases := map[string]bool{
		"NAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\n": true,
		"ID=debian\n": true,
		"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n": true,
		"ID=fedora\n": false,
		"ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n": false,
	}
	for release, want := range cases {
		if got := debianLike(release); got != want {
			t.Errorf("debianLike(%q) = %v", release, got)
		}
	}
}

func TestWriteFileSetsModeAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "token")
	for _, content := range []string{"old\n", "new\n"} {
		if err := writeFile(WriteFile{Path: path, Mode: 0o600, Content: []byte(content)}); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path)
	data, _ := os.ReadFile(path)
	if err != nil || info.Mode().Perm() != 0o600 || string(data) != "new\n" {
		t.Errorf("token file: mode %v, content %q, err %v", info.Mode(), data, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("left %d files behind", len(entries))
	}
}
