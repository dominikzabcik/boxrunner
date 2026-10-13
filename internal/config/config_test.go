package config

import (
	"reflect"
	"testing"
)

func TestParseBytes(t *testing.T) {
	cases := map[string]Bytes{
		"10GB":     10_000_000_000,
		"6GiB":     6 << 30,
		"512 MiB":  512 << 20,
		"1.5GiB":   3 << 29,
		"2048":     2048,
		"1gib":     1 << 30,
		"0":        0,
		"7tb":      7_000_000_000_000,
		"64KiB":    64 << 10,
		"1000000B": 1_000_000,
	}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	bad := map[string]string{
		"6G":    `"6G" has an unknown unit "G" (use B, KB, MB, GB, TB, KiB, MiB, GiB or TiB)`,
		"-1GiB": `"-1GiB" is not a size like 6GiB or 10GB`,
		"lots":  `"lots" is not a size like 6GiB or 10GB`,
	}
	for in, want := range bad {
		if _, err := ParseBytes(in); err == nil || err.Error() != want {
			t.Errorf("ParseBytes(%q) error = %v; want %s", in, err, want)
		}
	}
}

func TestParseFull(t *testing.T) {
	got, err := Parse([]byte(`
worker = "https://boxrunner.example.workers.dev/"
box = "box-1"
disk_floor = "20GB"

[[class]]
name = "boxrunner"
cpus = 4
memory = "6GiB"
priority = "normal"
max = 1

[[class]]
name = "boxrunner-light"
cpus = 1.5
memory = "2560MiB"
pids = 512
shm = "256MiB"
priority = "idle"
max = 3
image = "boxrunner/runner-node:local"
`), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if got.Worker.String() != "https://boxrunner.example.workers.dev" {
		t.Errorf("worker = %s", got.Worker)
	}
	got.Worker = nil
	want := Config{
		Box:       "box-1",
		DiskFloor: 20_000_000_000,
		Classes: []Class{
			{Name: "boxrunner", NanoCPUs: 4_000_000_000, Memory: 6 << 30, Pids: 4096, Shm: 1 << 30, Priority: Normal, Max: 1, Image: "boxrunner/runner:local"},
			{Name: "boxrunner-light", NanoCPUs: 1_500_000_000, Memory: 2560 << 20, Pids: 512, Shm: 256 << 20, Priority: Idle, Max: 3, Image: "boxrunner/runner-node:local"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseBoxFromHostname(t *testing.T) {
	got, err := Parse([]byte("worker = \"http://127.0.0.1:8787\"\n[[class]]\nname = \"a\"\ncpus = 1\nmemory = \"1GiB\"\nmax = 1\n"), "Build_Box.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Box != "build-box" || got.DiskFloor != 10_000_000_000 {
		t.Errorf("box = %q, disk floor = %d", got.Box, got.DiskFloor)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, file, want string
	}{
		{
			name: "every field wrong",
			file: `
worker = "http://boxrunner.example.com"
box = "Box 1"
disk_floor = "ten"
colour = "blue"

[[class]]
name = "Big"
cpus = 0
memory = "100MiB"
priority = "low"
max = 0

[[class]]
cpus = 1
memory = "1GiB"
max = 1
`,
			want: `config:
unknown key "colour"
worker: "http://boxrunner.example.com" must use https
box: "Box 1" must be lower-case letters, digits and dashes
disk_floor: "ten" is not a size like 6GiB or 10GB
class "Big": name: "Big" must be lower-case letters, digits and dashes
class "Big": cpus: must be more than 0
class "Big": memory: 100MiB is less than 256MiB
class "Big": priority: "low" must be "normal" or "idle"
class "Big": max: must be at least 1; remove the class to turn it off
class 2: name: missing`,
		},
		{
			name: "no worker, no class",
			file: `box = "box-1"`,
			want: `config:
worker: missing; it is the Worker's URL, such as https://boxrunner.example.workers.dev
no [[class]]: at least one is needed`,
		},
		{
			name: "duplicate class and long name",
			file: `
worker = "https://w.example"
box = "a-very-long-box-name-that-takes-most-of-the-room"
[[class]]
name = "boxrunner"
cpus = 1
memory = "1GiB"
max = 1
[[class]]
name = "boxrunner"
cpus = 1
memory = "1GiB"
max = 1
`,
			want: `config:
class "boxrunner": name: box and class names together must stay under 56 characters
class "boxrunner": name: box and class names together must stay under 56 characters
class "boxrunner": appears twice`,
		},
		{
			name: "not toml",
			file: `worker = `,
			want: `config: toml: line 1 (last key "worker"): unexpected EOF; expected value`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.file), "box-1")
			if err == nil || err.Error() != c.want {
				t.Errorf("error =\n%v\nwant\n%s", err, c.want)
			}
		})
	}
}

func TestDefaultParses(t *testing.T) {
	file := Default("https://w.example", "box-1", Host{CPUs: 4, Memory: 7_700_000_000})
	got, err := Parse([]byte(file), "unused")
	if err != nil {
		t.Fatal(err)
	}
	want := []Class{
		{Name: "boxrunner", NanoCPUs: 4e9, Memory: 5376 << 20, Pids: 4096, Shm: 1 << 30, Priority: Normal, Max: 1, Image: "boxrunner/runner:local"},
		{Name: "boxrunner-light", NanoCPUs: 4e9, Memory: 2816 << 20, Pids: 4096, Shm: 1 << 30, Priority: Idle, Max: 2, Image: "boxrunner/runner:local"},
	}
	if !reflect.DeepEqual(got.Classes, want) || got.Box != "box-1" || got.Worker.String() != "https://w.example" {
		t.Errorf("Default parsed to %+v", got)
	}
}

func TestSetTopLevel(t *testing.T) {
	file := "# comment\nworker = \"https://old.example\"\n\n[[class]]\nname = \"a\"\n"
	cases := []struct{ key, value, want string }{
		{"worker", "https://new.example", "# comment\nworker = \"https://new.example\"\n\n[[class]]\nname = \"a\"\n"},
		{"box", "box-2", "# comment\nworker = \"https://old.example\"\nbox = \"box-2\"\n\n[[class]]\nname = \"a\"\n"},
		{"worker", "https://old.example", file},
	}
	for _, c := range cases {
		if got := SetTopLevel(file, c.key, c.value); got != c.want {
			t.Errorf("SetTopLevel(%s=%s) =\n%q\nwant\n%q", c.key, c.value, got, c.want)
		}
	}
	if got := SetTopLevel("", "box", "b"); got != "box = \"b\"\n" {
		t.Errorf("SetTopLevel on empty file = %q", got)
	}
}
