package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dominikzabcik/boxrunner/internal/docker"
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

func TestOSRelease(t *testing.T) {
	id, version := osRelease("PRETTY_NAME=\"Ubuntu 24.04.3 LTS\"\nID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n")
	if id != "ubuntu" || version != "24.04" {
		t.Errorf("osRelease = %q, %q", id, version)
	}
}

func TestAtLeast(t *testing.T) {
	cases := []struct {
		version, min string
		want         bool
	}{
		{"6.8.0-45-generic", "5.15", true},
		{"5.15.0-1060-oracle", "5.15", true},
		{"5.10.0-28-amd64", "5.15", false},
		{"7.0", "5.15", true},
		{"22.04", "22.04", true},
		{"20.04", "22.04", false},
		{"12", "11", true},
		{"", "11", false},
	}
	for _, c := range cases {
		if got := atLeast(c.version, c.min); got != c.want {
			t.Errorf("atLeast(%q, %q) = %v", c.version, c.min, got)
		}
	}
}

func TestWithNetworks(t *testing.T) {
	pools := []docker.AddressPool{{Base: "172.17.0.0/16", Size: 16}, {Base: "192.168.0.0/16", Size: 20}}
	cases := []struct {
		name, current, bip, want string
	}{
		{
			name: "no file",
			bip:  bridgeIP("172.17.0.1", "172.17.0.0/16"),
			want: `{
    "bip": "172.17.0.1/16",
    "default-address-pools": [
        {
            "base": "172.17.0.0/16",
            "size": 16
        },
        {
            "base": "192.168.0.0/16",
            "size": 20
        }
    ]
}
`,
		},
		{
			name:    "own settings kept",
			current: `{"exec-opts":["native.cgroupdriver=systemd"],"bip":"10.10.0.1/24","default-address-pools":[{"base":"10.20.0.0/16","size":24}]}`,
			bip:     "172.17.0.1/16",
			want: `{
    "bip": "10.10.0.1/24",
    "default-address-pools": [
        {
            "base": "10.20.0.0/16",
            "size": 24
        }
    ],
    "exec-opts": [
        "native.cgroupdriver=systemd"
    ]
}
`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := withNetworks([]byte(c.current), c.bip, pools)
			if err != nil || string(got) != c.want {
				t.Errorf("withNetworks =\n%s\nwant\n%s (%v)", got, c.want, err)
			}
		})
	}
	if _, err := withNetworks([]byte("{oops"), "", nil); err == nil || err.Error() != "/etc/docker/daemon.json: invalid character 'o' looking for beginning of object key string" {
		t.Errorf("broken daemon.json: %v", err)
	}
}
