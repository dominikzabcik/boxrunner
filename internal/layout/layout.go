// Package layout names every path, unit, label and Docker object boxrunner
// creates on a box, so install, run and uninstall agree on them.
package layout

const (
	ConfigDir   = "/etc/boxrunner"
	ConfigPath  = "/etc/boxrunner/config.toml"
	TokenPath   = "/etc/boxrunner/token"
	StateDir    = "/var/lib/boxrunner"
	JobsLogPath = "/var/lib/boxrunner/jobs.jsonl"
	// DestinationsPath holds the destinations jobs connected to, per repository.
	DestinationsPath = "/var/lib/boxrunner/destinations.json"
	// DrainPath exists while the box drains, so a drain outlasts a restart.
	DrainPath = "/var/lib/boxrunner/draining"
	Socket    = "/run/boxrunner.sock"
	Binary    = "/usr/local/bin/boxrunner"
	UnitName  = "boxrunner.service"
	UnitPath  = "/etc/systemd/system/boxrunner.service"
	// A dash in a slice name means nesting to systemd, so the idle slice uses
	// an underscore to stay at the top level (docs/decisions.md).
	IdleSlice     = "boxrunner_idle.slice"
	IdleSlicePath = "/etc/systemd/system/boxrunner_idle.slice"
	CgroupDir     = "/sys/fs/cgroup"
	DockerSocket  = "/var/run/docker.sock"
	Network       = "boxrunner"
	Bridge        = "boxrunner0"
	Image         = "boxrunner/runner:local"
	// SysboxRuntime is the Docker runtime that `install --docker` registers.
	SysboxRuntime = "sysbox-runc"
	// EnvDocker tells the job image's entrypoint to start a Docker daemon.
	EnvDocker = "BOXRUNNER_DOCKER=1"
	// EnvMirror gives that daemon the registry mirror's URL.
	EnvMirror = "BOXRUNNER_MIRROR"
	// SysboxDir holds what Sysbox keeps for each container, one directory per
	// kind and container id; docker/<id> is the job's inner /var/lib/docker.
	SysboxDir = "/var/lib/sysbox"
	// Mirror names the registry mirror's container and its cache volume. It
	// listens on the bridge's gateway address only.
	Mirror     = "boxrunner-mirror"
	MirrorPort = 5000
	// CachePort is where the agent serves the Actions cache to jobs, on the
	// bridge's gateway address only.
	CachePort = 5001
	// CacheDir holds the Actions cache's archives.
	CacheDir = "/var/lib/boxrunner/cache"
	// EnvCache gives jobs of a class with cache = true the cache's URL.
	EnvCache = "BOXRUNNER_CACHE"
	// EnvDNS gives the Docker daemon of a job in an egress class the address
	// of the class's DNS forwarder.
	EnvDNS = "BOXRUNNER_DNS"
	// NFLogGroup is where the egress rules log the connections they drop.
	NFLogGroup = 4137
	// SeenNFLogGroup is where the rules of the destinations log send the
	// start of each new connection that leaves a job network.
	SeenNFLogGroup = 4138

	LabelBox        = "boxrunner.box"
	LabelRunner     = "boxrunner.runner"
	LabelRunnerID   = "boxrunner.runner-id"
	LabelRepo       = "boxrunner.repo"
	LabelClass      = "boxrunner.class"
	LabelScaleSetID = "boxrunner.scaleset-id"
	LabelVersion    = "boxrunner.runner-version"
	// LabelImage holds the job image's identity: runner version and build context.
	LabelImage = "boxrunner.image"
	// LabelDigest holds the digest of the runner image the job image is built
	// from, which boxrunner's CI verified.
	LabelDigest = "boxrunner.runner-digest"
	// LabelMirror marks the registry mirror's container and volume; on the
	// container it holds the mirror's identity: image and address.
	LabelMirror = "boxrunner.mirror"
	// LabelEgress marks the network of an egress class; it holds the class's name.
	LabelEgress = "boxrunner.egress"

	// ExitConfig is the exit status for a problem a restart cannot fix (a wrong
	// token, an agent too old for its Worker); the unit does not restart on it.
	ExitConfig = 78
)
