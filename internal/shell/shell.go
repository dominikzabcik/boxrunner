// Package shell opens a shell in a job's container: the running job itself,
// or a copy of a failed job's container that its class keeps for inspection.
package shell

import (
	"fmt"
	"strings"
)

// Job is one job container as `docker ps` lists it.
type Job struct {
	Name, State, Repo, Class, Status string
}

// Running reports whether the job still runs, so the shell joins it.
func (j Job) Running() bool { return j.State == "running" }

// Format is the docker ps --format template Parse reads.
const Format = `{{.Names}}	{{.State}}	{{.Label "boxrunner.repo"}}	{{.Label "boxrunner.class"}}	{{.Status}}`

// Parse reads docker ps output written with Format.
func Parse(out string) []Job {
	var jobs []Job
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 5 {
			jobs = append(jobs, Job{Name: f[0], State: f[1], Repo: f[2], Class: f[3], Status: f[4]})
		}
	}
	return jobs
}

// Pick finds the job a name refers to: the whole runner name, or a part of
// it that only one job's name contains.
func Pick(jobs []Job, name string) (Job, error) {
	var found []Job
	for _, j := range jobs {
		if j.Name == name {
			return j, nil
		}
		if strings.Contains(j.Name, name) {
			found = append(found, j)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return Job{}, fmt.Errorf("no job container matches %q; run boxrunner shell to list them", name)
	default:
		names := make([]string, len(found))
		for i, j := range found {
			names[i] = j.Name
		}
		return Job{}, fmt.Errorf("%q matches %s; use more of the name", name, strings.Join(names, ", "))
	}
}

// Join is the docker command that opens a shell in a running job.
func Join(j Job) []string {
	return []string{"exec", "-it", "-u", "runner", "-w", "/home/runner/_work", j.Name, "bash"}
}

// Inspect is the docker command that opens a shell in a snapshot of a
// stopped job, on the jobs' network and with modest limits.
func Inspect(image string) []string {
	return []string{"run", "-it", "--rm", "--network", "boxrunner", "--cpus", "1", "--memory", "1g", "--pids-limit", "512",
		"-u", "runner", "-w", "/home/runner/_work", "--entrypoint", "bash", image}
}
