// Package layout names every path, unit, label and Docker object boxrunner
// creates on a box, so install, run and uninstall agree on them.
package layout

const (
	ConfigDir   = "/etc/boxrunner"
	ConfigPath  = "/etc/boxrunner/config.toml"
	TokenPath   = "/etc/boxrunner/token"
	StateDir    = "/var/lib/boxrunner"
	JobsLogPath = "/var/lib/boxrunner/jobs.jsonl"
	Socket      = "/run/boxrunner.sock"
	Binary      = "/usr/local/bin/boxrunner"
	UnitName    = "boxrunner.service"
	UnitPath    = "/etc/systemd/system/boxrunner.service"
	// A dash in a slice name means nesting to systemd, so the idle slice uses
	// an underscore to stay at the top level (docs/decisions.md).
	IdleSlice     = "boxrunner_idle.slice"
	IdleSlicePath = "/etc/systemd/system/boxrunner_idle.slice"
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
	// Mirror names the registry mirror's container and its cache volume. It
	// listens on the bridge's gateway address only.
	Mirror     = "boxrunner-mirror"
	MirrorPort = 5000

	LabelBox        = "boxrunner.box"
	LabelRunner     = "boxrunner.runner"
	LabelRunnerID   = "boxrunner.runner-id"
	LabelRepo       = "boxrunner.repo"
	LabelClass      = "boxrunner.class"
	LabelScaleSetID = "boxrunner.scaleset-id"
	LabelVersion    = "boxrunner.runner-version"
	// LabelImage holds the job image's identity: runner version and build context.
	LabelImage = "boxrunner.image"
	// LabelMirror marks the registry mirror's container and volume; on the
	// container it holds the mirror's identity: image and address.
	LabelMirror = "boxrunner.mirror"

	// ExitConfig is the exit status for a problem a restart cannot fix (a wrong
	// token, an agent too old for its Worker); the unit does not restart on it.
	ExitConfig = 78
)
