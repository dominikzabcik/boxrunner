// Package config parses /etc/boxrunner/config.toml into typed values. Every
// check happens here; the rest of the agent trusts a Config.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/dominikzabcik/boxrunner/internal/layout"
)

type Priority int

const (
	Normal Priority = iota
	Idle
)

func (p Priority) String() string {
	if p == Idle {
		return "idle"
	}
	return "normal"
}

// Isolation is how a class's jobs are kept from the host.
type Isolation int

const (
	// AutoIsolation is the class's choice when it makes none: Sysbox on a box
	// that has it, runc otherwise. Config.ResolveIsolation settles it.
	AutoIsolation Isolation = iota
	// Sysbox runs jobs under sysbox-runc, where a job's root is an
	// unprivileged user on the host.
	Sysbox
	// Runc runs jobs under Docker's default runtime, where a job's root is
	// root on the host.
	Runc
)

func (i Isolation) String() string {
	switch i {
	case Sysbox:
		return "sysbox"
	case Runc:
		return "runc"
	}
	return "auto"
}

// Bytes is a size in bytes.
type Bytes int64

const (
	KiB Bytes = 1 << 10
	MiB Bytes = 1 << 20
	GiB Bytes = 1 << 30
	GB  Bytes = 1_000_000_000
)

var units = map[string]Bytes{
	"": 1, "b": 1,
	"kb": 1_000, "mb": 1_000_000, "gb": GB, "tb": 1_000 * GB,
	"kib": KiB, "mib": MiB, "gib": GiB, "tib": 1 << 40,
}

var sizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([a-zA-Z]*)$`)

// ParseBytes reads "10GB" (decimal), "6GiB" (binary) or a plain byte count.
func ParseBytes(s string) (Bytes, error) {
	m := sizePattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("%q is not a size like 6GiB or 10GB", s)
	}
	unit, ok := units[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("%q has an unknown unit %q (use B, KB, MB, GB, TB, KiB, MiB, GiB or TiB)", s, m[2])
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n*float64(unit) > math.MaxInt64 {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return Bytes(math.Round(n * float64(unit))), nil
}

// String writes the size in the largest binary unit that divides it.
func (b Bytes) String() string {
	switch {
	case b != 0 && b%GiB == 0:
		return fmt.Sprintf("%dGiB", b/GiB)
	case b != 0 && b%MiB == 0:
		return fmt.Sprintf("%dMiB", b/MiB)
	default:
		return fmt.Sprintf("%dB", int64(b))
	}
}

type Class struct {
	// Name is the runs-on label and the scale set's name.
	Name     string
	NanoCPUs int64
	Memory   Bytes
	Pids     int64
	Shm      Bytes
	Priority Priority
	// Max is how many runners of this class may start or run at once.
	Max   int
	Image string
	// KeepFailed keeps a failed job's container this long for boxrunner shell.
	KeepFailed time.Duration
	// MaxDuration stops a job's container that runs longer; 0 never does.
	MaxDuration time.Duration
	// Repos, when set, limits the class to these repositories (owner/name,
	// lower case); otherwise it serves every repository the Worker lists.
	Repos []string
	// Warm names repositories (owner/name, lower case) that each keep one
	// idle runner of the class ready.
	Warm []string
	// Dockerfile, when set, builds Image FROM the job image; the agent
	// rebuilds it whenever the job image or the Dockerfile's directory changes.
	Dockerfile string
	// Docker gives each job its own Docker daemon. Such a class's Isolation
	// is always Sysbox.
	Docker    bool
	Isolation Isolation
}

type Config struct {
	Worker    *url.URL
	Box       string
	DiskFloor Bytes
	Classes   []Class
}

func (c Config) Class(name string) (Class, bool) {
	i := slices.IndexFunc(c.Classes, func(k Class) bool { return k.Name == name })
	if i < 0 {
		return Class{}, false
	}
	return c.Classes[i], true
}

const (
	DefaultDiskFloor = 10 * GB
	DefaultPids      = 4096
	DefaultShm       = 1 * GiB
	// A runner's name is "<box>-<class>-<6 hex>" and GitHub allows 64 characters.
	maxRunnerName = 64
	nameSuffix    = 8
	minMemory     = 256 * MiB
)

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type rawConfig struct {
	Worker    string     `toml:"worker"`
	Box       string     `toml:"box"`
	DiskFloor string     `toml:"disk_floor"`
	Classes   []rawClass `toml:"class"`
}

type rawClass struct {
	Name        string   `toml:"name"`
	CPUs        float64  `toml:"cpus"`
	Memory      string   `toml:"memory"`
	Pids        int64    `toml:"pids"`
	Shm         string   `toml:"shm"`
	Priority    string   `toml:"priority"`
	Max         int      `toml:"max"`
	Image       string   `toml:"image"`
	Dockerfile  string   `toml:"dockerfile"`
	Repos       []string `toml:"repos"`
	Warm        []string `toml:"warm"`
	KeepFailed  string   `toml:"keep_failed"`
	MaxDuration string   `toml:"max_duration"`
	Docker      bool     `toml:"docker"`
	Isolation   string   `toml:"isolation"`
}

// Parse reads a config file. hostname names the box when the file does not.
func Parse(data []byte, hostname string) (Config, error) {
	var raw rawConfig
	meta, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&raw)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	var errs []error
	for _, key := range meta.Undecoded() {
		errs = append(errs, fmt.Errorf("unknown key %q", key.String()))
	}

	var cfg Config
	if cfg.Worker, err = parseWorker(raw.Worker); err != nil {
		errs = append(errs, err)
	}

	cfg.Box = raw.Box
	if cfg.Box == "" {
		cfg.Box = BoxName(hostname)
	}
	if !namePattern.MatchString(cfg.Box) {
		errs = append(errs, fmt.Errorf("box: %q must be lower-case letters, digits and dashes", cfg.Box))
	}

	cfg.DiskFloor = DefaultDiskFloor
	if raw.DiskFloor != "" {
		if cfg.DiskFloor, err = ParseBytes(raw.DiskFloor); err != nil {
			errs = append(errs, fmt.Errorf("disk_floor: %w", err))
		}
	}

	if len(raw.Classes) == 0 {
		errs = append(errs, errors.New("no [[class]]: at least one is needed"))
	}
	seen := map[string]bool{}
	for i, rc := range raw.Classes {
		class, classErrs := parseClass(rc, cfg.Box)
		label := fmt.Sprintf("class %d", i+1)
		if rc.Name != "" {
			label = fmt.Sprintf("class %q", rc.Name)
		}
		if seen[rc.Name] && rc.Name != "" {
			classErrs = append(classErrs, errors.New("appears twice"))
		}
		seen[rc.Name] = true
		for _, e := range classErrs {
			errs = append(errs, fmt.Errorf("%s: %w", label, e))
		}
		cfg.Classes = append(cfg.Classes, class)
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("config:\n%w", err)
	}
	return cfg, nil
}

func parseWorker(s string) (*url.URL, error) {
	if s == "" {
		return nil, errors.New("worker: missing; it is the Worker's URL, such as https://boxrunner.example.workers.dev")
	}
	u, err := url.Parse(strings.TrimRight(s, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("worker: %q is not a URL", s)
	}
	loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, fmt.Errorf("worker: %q must use https", s)
	}
	return u, nil
}

func parseClass(rc rawClass, box string) (Class, []error) {
	var errs []error
	c := Class{
		Name:     rc.Name,
		NanoCPUs: int64(math.Round(rc.CPUs * 1e9)),
		Pids:     rc.Pids,
		Max:      rc.Max,
		Image:    rc.Image,
		Shm:      DefaultShm,
		Docker:   rc.Docker,
	}
	switch {
	case rc.Name == "":
		errs = append(errs, errors.New("name: missing"))
	case !namePattern.MatchString(rc.Name):
		errs = append(errs, fmt.Errorf("name: %q must be lower-case letters, digits and dashes", rc.Name))
	case len(box)+1+len(rc.Name)+nameSuffix > maxRunnerName:
		errs = append(errs, fmt.Errorf("name: box and class names together must stay under %d characters", maxRunnerName-nameSuffix))
	}
	if rc.CPUs <= 0 {
		errs = append(errs, errors.New("cpus: must be more than 0"))
	}
	var err error
	if rc.Memory == "" {
		errs = append(errs, errors.New("memory: missing"))
	} else if c.Memory, err = ParseBytes(rc.Memory); err != nil {
		errs = append(errs, fmt.Errorf("memory: %w", err))
	} else if c.Memory < minMemory {
		errs = append(errs, fmt.Errorf("memory: %s is less than %s", c.Memory, minMemory))
	}
	if rc.Shm != "" {
		if c.Shm, err = ParseBytes(rc.Shm); err != nil {
			errs = append(errs, fmt.Errorf("shm: %w", err))
		}
	}
	if c.Pids == 0 {
		c.Pids = DefaultPids
	} else if c.Pids < 0 {
		errs = append(errs, errors.New("pids: must be more than 0"))
	}
	switch rc.Priority {
	case "", "normal":
		c.Priority = Normal
	case "idle":
		c.Priority = Idle
	default:
		errs = append(errs, fmt.Errorf("priority: %q must be \"normal\" or \"idle\"", rc.Priority))
	}
	switch rc.Isolation {
	case "":
		if c.Docker {
			c.Isolation = Sysbox
		}
	case "sysbox":
		c.Isolation = Sysbox
	case "runc":
		c.Isolation = Runc
		if c.Docker {
			errs = append(errs, errors.New(`isolation: "runc" cannot give jobs their own Docker; docker = true needs "sysbox"`))
		}
	default:
		errs = append(errs, fmt.Errorf("isolation: %q must be \"sysbox\" or \"runc\"", rc.Isolation))
	}
	if rc.Max < 1 {
		errs = append(errs, errors.New("max: must be at least 1; remove the class to turn it off"))
	}
	if rc.KeepFailed != "" {
		d, err := time.ParseDuration(rc.KeepFailed)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("keep_failed: %q is not a duration like 15m or 2h", rc.KeepFailed))
		case d < 0 || d > 24*time.Hour:
			errs = append(errs, fmt.Errorf("keep_failed: %s must be between 0 and 24h", rc.KeepFailed))
		default:
			c.KeepFailed = d
		}
	}
	if rc.MaxDuration != "" {
		d, err := time.ParseDuration(rc.MaxDuration)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("max_duration: %q is not a duration like 90m or 6h", rc.MaxDuration))
		case d < time.Minute:
			errs = append(errs, fmt.Errorf("max_duration: %s must be at least 1m", rc.MaxDuration))
		default:
			c.MaxDuration = d
		}
	}
	for _, r := range rc.Repos {
		if owner, name, ok := strings.Cut(r, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			errs = append(errs, fmt.Errorf("repos: %q must be owner/name", r))
			continue
		}
		c.Repos = append(c.Repos, strings.ToLower(r))
	}
	for _, r := range rc.Warm {
		switch owner, name, ok := strings.Cut(r, "/"); {
		case !ok || owner == "" || name == "" || strings.Contains(name, "/"):
			errs = append(errs, fmt.Errorf("warm: %q must be owner/name", r))
		case !c.Serves(r):
			errs = append(errs, fmt.Errorf("warm: %q is not in the class's repos", r))
		case !slices.Contains(c.Warm, strings.ToLower(r)):
			c.Warm = append(c.Warm, strings.ToLower(r))
		}
	}
	// A warm runner is never removed for idleness, so the class's other
	// repositories need a place of their own.
	onlyWarm := len(c.Repos) > 0 && len(c.Repos) == len(c.Warm)
	switch {
	case c.Max < 1:
	case len(c.Warm) > c.Max:
		errs = append(errs, fmt.Errorf("warm: %d repositories need more idle runners than max (%d)", len(c.Warm), c.Max))
	case len(c.Warm) == c.Max && !onlyWarm:
		errs = append(errs, fmt.Errorf("warm: the warm runners fill max (%d) and leave no place for the class's other repositories; raise max or list only the warm repositories in repos", c.Max))
	}
	switch {
	case rc.Dockerfile != "" && rc.Image != "":
		errs = append(errs, errors.New("image and dockerfile: set one; a dockerfile names its own image"))
	case rc.Dockerfile != "" && !filepath.IsAbs(rc.Dockerfile):
		errs = append(errs, fmt.Errorf("dockerfile: %q must be an absolute path", rc.Dockerfile))
	case rc.Dockerfile != "":
		c.Dockerfile = filepath.Clean(rc.Dockerfile)
		c.Image = ClassImage(rc.Name)
	case c.Image == "":
		c.Image = layout.Image
	}
	return c, errs
}

// ResolveIsolation settles the isolation of the classes that leave it unset:
// Sysbox when the box has Sysbox's runtime, runc otherwise.
func (c Config) ResolveIsolation(sysbox bool) Config {
	c.Classes = slices.Clone(c.Classes)
	for i := range c.Classes {
		if c.Classes[i].Isolation != AutoIsolation {
			continue
		}
		c.Classes[i].Isolation = Runc
		if sysbox {
			c.Classes[i].Isolation = Sysbox
		}
	}
	return c
}

// Serves reports whether the class runs jobs of a repository.
func (c Class) Serves(repo string) bool {
	return len(c.Repos) == 0 || slices.Contains(c.Repos, strings.ToLower(repo))
}

// ClassImage is the tag of the image a class's dockerfile builds.
func ClassImage(class string) string { return "boxrunner/class-" + class + ":local" }

// BoxName turns a hostname into a box name: its first label, lower case,
// with anything outside [a-z0-9-] replaced by a dash.
func BoxName(hostname string) string {
	label, _, _ := strings.Cut(strings.ToLower(hostname), ".")
	name := []byte(label)
	for i, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			name[i] = '-'
		}
	}
	return strings.Trim(string(name), "-")
}
