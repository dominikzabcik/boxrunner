// Command boxrunner runs GitHub Actions jobs in fresh containers on a box.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/dominikzabcik/boxrunner/internal/bench"
	"github.com/dominikzabcik/boxrunner/internal/shell"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/agent"
	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/doctor"
	"github.com/dominikzabcik/boxrunner/internal/install"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/status"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "0.1.0-dev"

const usage = `boxrunner runs GitHub Actions jobs in fresh containers on this box.

Usage:
  boxrunner install [--worker URL] [--token TOKEN] [--box NAME] [--docker] [--sysbox]
  boxrunner run
  boxrunner status
  boxrunner doctor
  boxrunner bench [--threads N] [--rounds N]
  boxrunner shell [RUNNER]
  boxrunner uninstall
  boxrunner version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "install":
		err = installCmd(ctx, os.Args[2:])
	case "run":
		err = runCmd(ctx)
	case "status":
		err = statusCmd(ctx)
	case "doctor":
		err = doctorCmd(ctx)
	case "bench":
		err = benchCmd(ctx, os.Args[2:])
	case "shell":
		err = shellCmd(ctx, os.Args[2:])
	case "uninstall":
		err = uninstallCmd(ctx)
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "boxrunner: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	var exit exitError
	switch {
	case errors.As(err, &exit):
		fmt.Fprintln(os.Stderr, "boxrunner:", exit.err)
		os.Exit(exit.code)
	case err != nil:
		fmt.Fprintln(os.Stderr, "boxrunner:", err)
		os.Exit(1)
	}
}

type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }

func loadConfig() (config.Config, string, error) {
	hostname, _ := os.Hostname()
	data, err := os.ReadFile(layout.ConfigPath)
	if err != nil {
		return config.Config{}, "", fmt.Errorf("%w; run boxrunner install", err)
	}
	cfg, err := config.Parse(data, hostname)
	if err != nil {
		return config.Config{}, "", err
	}
	token, err := os.ReadFile(layout.TokenPath)
	if err != nil {
		return cfg, "", fmt.Errorf("%w; run boxrunner install --token …", err)
	}
	return cfg, strings.TrimSpace(string(token)), nil
}

func runCmd(ctx context.Context) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, token, err := loadConfig()
	if err != nil {
		return exitError{layout.ExitConfig, err}
	}
	d, err := docker.Dial(ctx, layout.DockerSocket)
	if err != nil {
		return err
	}
	w := worker.New(worker.Options{URL: cfg.Worker, Token: token, Version: version, Log: log})
	err = agent.Run(ctx, agent.Options{Config: cfg, Worker: w, Docker: d, Run: install.Exec, Version: version, Log: log})
	if !worker.Fatal(err) {
		return err
	}
	switch {
	case worker.Is(err, worker.Unauthorized):
		err = fmt.Errorf("the Worker refused the box token in %s: %w", layout.TokenPath, err)
	case worker.Is(err, worker.UpgradeRequired):
		err = fmt.Errorf("this agent (%s) does not match the Worker; install the matching boxrunner and run `sudo boxrunner install`: %w", version, err)
	case worker.Is(err, worker.NotConfigured):
		err = fmt.Errorf("the Worker's setup is unfinished; open %s/setup: %w", cfg.Worker, err)
	}
	return exitError{layout.ExitConfig, err}
}

func installCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("install", flag.ExitOnError)
	workerURL := flags.String("worker", os.Getenv("BOXRUNNER_WORKER"), "the Worker's URL (or BOXRUNNER_WORKER)")
	token := flags.String("token", os.Getenv("BOXRUNNER_TOKEN"), "the box token from the Worker's setup page (or BOXRUNNER_TOKEN)")
	box := flags.String("box", os.Getenv("BOXRUNNER_BOX"), "this box's name, [a-z0-9-] (or BOXRUNNER_BOX; default: the hostname)")
	withDocker := flags.Bool("docker", os.Getenv("BOXRUNNER_DOCKER") == "1", "install Sysbox so jobs can run Docker, and turn it on for the classes of a new config (or BOXRUNNER_DOCKER=1)")
	withSysbox := flags.Bool("sysbox", os.Getenv("BOXRUNNER_SYSBOX") == "1", "install Sysbox so a job's root is an unprivileged user on the box, without Docker in jobs (or BOXRUNNER_SYSBOX=1)")
	flags.Parse(args)
	if os.Geteuid() != 0 {
		return errors.New("install changes the system; run it with sudo")
	}
	if _, err := os.Stat(layout.ConfigPath); err != nil && *workerURL == "" {
		*workerURL = prompt("Worker URL", false)
	}
	if _, err := os.Stat(layout.TokenPath); err != nil && *token == "" {
		*token = prompt("Box token", true)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	memory, err := install.HostMemory()
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	steps, err := install.Install(ctx, install.Desired{
		Worker: *workerURL, Token: strings.TrimSpace(*token), Box: *box, Hostname: hostname,
		Host:   config.Host{CPUs: runtime.NumCPU(), Memory: config.Bytes(memory)},
		Binary: binary, Docker: *withDocker, Sysbox: *withSysbox,
	}, install.Exec, os.Stdout)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		fmt.Println("No changes: the box is already set up.")
	} else {
		fmt.Printf("Done in %d steps. Check the box with: sudo boxrunner doctor\n", len(steps))
	}
	return nil
}

// prompt asks on the terminal, if there is one; secret input is not echoed.
func prompt(label string, secret bool) string {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return ""
	}
	defer tty.Close()
	fmt.Fprintf(tty, "%s: ", label)
	if secret {
		stty := func(arg string) {
			cmd := exec.Command("stty", arg)
			cmd.Stdin = tty
			cmd.Run()
		}
		stty("-echo")
		defer func() {
			stty("echo")
			fmt.Fprintln(tty)
		}()
	}
	line, _ := bufio.NewReader(tty).ReadString('\n')
	return strings.TrimSpace(line)
}

func uninstallCmd(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("uninstall changes the system; run it with sudo")
	}
	steps, err := install.Uninstall(ctx, install.Exec, os.Stdout)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		fmt.Println("Nothing to remove.")
	} else {
		fmt.Println("boxrunner is gone. Docker stays installed, and so does Sysbox if install added it.")
	}
	return nil
}

func statusCmd(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s, err := status.Fetch(ctx, layout.Socket)
	if err != nil {
		return err
	}
	status.Print(os.Stdout, s, time.Now())
	return nil
}

func doctorCmd(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cfg, token, err := loadConfig()
	checks := doctor.Checks{Config: cfg, ConfigErr: err, Exec: install.Exec, HTTP: &http.Client{Timeout: 15 * time.Second}}
	if err == nil {
		checks.Worker = worker.New(worker.Options{URL: cfg.Worker, Token: token, Version: version,
			HTTP: &http.Client{Timeout: 15 * time.Second}})
	}
	failed := 0
	for _, r := range checks.All(ctx) {
		mark := "ok  "
		if !r.OK {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("%s  %-15s %s\n", mark, r.Name, r.Detail)
	}
	if failed > 0 {
		return exitError{1, fmt.Errorf("%d checks failed", failed)}
	}
	return nil
}

func benchCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	threads := fs.Int("threads", runtime.NumCPU(), "CPU-bound processes in the timed job, and busy containers beside it")
	rounds := fs.Int("rounds", 3, "runs per scenario; the report shows the median")
	iterations := fs.Int("iterations", 15_000_000, "loop iterations per process")
	fs.Parse(args)
	docker := func(ctx context.Context, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("docker %s: %s", args[0], strings.TrimSpace(string(exit.Stderr)))
		}
		return string(out), err
	}
	fmt.Fprintf(os.Stderr, "Timing %d threads alone, then beside %d busy containers at idle and at normal priority. This takes a few minutes.\n", *threads, *threads)
	results, err := bench.Measure(ctx, docker, layout.Image, *threads, *iterations, *rounds)
	if err != nil {
		return err
	}
	fmt.Print(bench.Report(results, *threads))
	return nil
}

func shellCmd(ctx context.Context, args []string) error {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "label="+layout.LabelBox, "--format", shell.Format).Output()
	if err != nil {
		return fmt.Errorf("listing job containers: %w", err)
	}
	jobs := shell.Parse(string(out))
	if len(args) == 0 {
		if len(jobs) == 0 {
			fmt.Println("No job containers right now. A class with keep_failed keeps failed ones; see docs/config.md.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "RUNNER\tREPOSITORY\tCLASS\tSTATE")
		for _, j := range jobs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", j.Name, j.Repo, j.Class, j.Status)
		}
		w.Flush()
		fmt.Println("\nOpen a shell with: sudo boxrunner shell RUNNER (any unique part of the name)")
		return nil
	}
	job, err := shell.Pick(jobs, args[0])
	if err != nil {
		return err
	}
	if job.Running() {
		return attach(ctx, shell.Join(job)...)
	}
	// A stopped container cannot run a shell, so open one in a snapshot of it.
	runtime, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.HostConfig.Runtime}}", job.Name).Output()
	if err != nil {
		return fmt.Errorf("docker inspect %s: %w", job.Name, err)
	}
	image := "boxrunner/inspect:" + job.Name
	if out, err := exec.CommandContext(ctx, "docker", "commit", job.Name, image).CombinedOutput(); err != nil {
		return fmt.Errorf("docker commit: %s", strings.TrimSpace(string(out)))
	}
	defer exec.Command("docker", "rmi", "-f", image).Run()
	fmt.Fprintf(os.Stderr, "%s has stopped; this shell runs in a copy of its container, which goes away when you exit.\n", job.Name)
	return attach(ctx, shell.Inspect(image, strings.TrimSpace(string(runtime)))...)
}

// attach runs docker with this terminal attached.
func attach(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}
