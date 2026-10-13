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

	LabelBox        = "boxrunner.box"
	LabelRunner     = "boxrunner.runner"
	LabelRunnerID   = "boxrunner.runner-id"
	LabelRepo       = "boxrunner.repo"
	LabelClass      = "boxrunner.class"
	LabelScaleSetID = "boxrunner.scaleset-id"
	LabelVersion    = "boxrunner.runner-version"

	// ExitConfig is the exit status for a problem a restart cannot fix (a wrong
	// token, an agent too old for its Worker); the unit does not restart on it.
	ExitConfig = 78
)
