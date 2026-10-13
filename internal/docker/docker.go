// Package docker is a small client for the few Docker Engine API calls the
// agent needs, over the unix socket.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxAPI is the newest API version this client speaks. Older daemons get
// their own version; every field used here exists since 1.41 (Docker 20.10).
const maxAPI = "1.47"

type Client struct {
	http *http.Client
	base string
	// API is the negotiated API version.
	API string
}

// Error is a Docker answer outside 2xx.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("docker: %d %s", e.Status, e.Message) }

func NotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// Dial connects to the daemon on a unix socket and agrees on an API version.
func Dial(ctx context.Context, socket string) (*Client, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	c := &Client{http: &http.Client{Transport: transport}, base: "http://docker"}
	var v struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := c.call(ctx, http.MethodGet, "/version", "", nil, &v); err != nil {
		return nil, fmt.Errorf("docker at %s: %w", socket, err)
	}
	c.API = maxAPI
	if older(v.APIVersion, maxAPI) {
		c.API = v.APIVersion
	}
	c.base = "http://docker/v" + c.API
	return c, nil
}

func older(a, b string) bool {
	parse := func(s string) (int, int) {
		major, minor, _ := strings.Cut(s, ".")
		x, _ := strconv.Atoi(major)
		y, _ := strconv.Atoi(minor)
		return x, y
	}
	am, an := parse(a)
	bm, bn := parse(b)
	return am < bm || am == bm && an < bn
}

type Info struct {
	ServerVersion string `json:"ServerVersion"`
	RootDir       string `json:"DockerRootDir"`
	CgroupDriver  string `json:"CgroupDriver"`
	CgroupVersion string `json:"CgroupVersion"`
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.call(ctx, http.MethodGet, "/info", "", nil, &out)
	return out, err
}

type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Labels  map[string]string `json:"Labels"`
	State   string            `json:"State"`
	Created int64             `json:"Created"`
	SizeRw  int64             `json:"SizeRw"`
}

func (c Container) Running() bool { return c.State == "running" }

func (c Container) CreatedAt() time.Time { return time.Unix(c.Created, 0).UTC() }

// List returns every container, running or not, that carries the label
// (key or key=value). With sizes, Docker also measures what each one wrote,
// which walks its files.
func (c *Client) List(ctx context.Context, label string, sizes bool) ([]Container, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	q := url.Values{"all": {"1"}, "filters": {string(filters)}}
	if sizes {
		q.Set("size", "1")
	}
	var out []Container
	err := c.call(ctx, http.MethodGet, "/containers/json?"+q.Encode(), "", nil, &out)
	return out, err
}

// ContainerSpec is everything boxrunner sets on a container.
type ContainerSpec struct {
	Name         string
	Image        string
	Cmd          []string
	Env          []string
	Labels       map[string]string
	NanoCPUs     int64
	Memory       int64
	PidsLimit    int64
	ShmSize      int64
	CgroupParent string
	Network      string
	Tty          bool
}

type hostConfig struct {
	NanoCpus     int64  `json:"NanoCpus,omitempty"`
	Memory       int64  `json:"Memory,omitempty"`
	MemorySwap   int64  `json:"MemorySwap,omitempty"`
	PidsLimit    int64  `json:"PidsLimit,omitempty"`
	ShmSize      int64  `json:"ShmSize,omitempty"`
	CgroupParent string `json:"CgroupParent,omitempty"`
	NetworkMode  string `json:"NetworkMode,omitempty"`
}

type createBody struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	Tty        bool              `json:"Tty,omitempty"`
	HostConfig hostConfig        `json:"HostConfig"`
}

// Create makes a container and returns its id. Memory and swap are set equal,
// so a container cannot swap.
func (c *Client) Create(ctx context.Context, s ContainerSpec) (string, error) {
	body := createBody{
		Image: s.Image, Cmd: s.Cmd, Env: s.Env, Labels: s.Labels, Tty: s.Tty,
		HostConfig: hostConfig{
			NanoCpus: s.NanoCPUs, Memory: s.Memory, MemorySwap: s.Memory, PidsLimit: s.PidsLimit,
			ShmSize: s.ShmSize, CgroupParent: s.CgroupParent, NetworkMode: s.Network,
		},
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(s.Name), "application/json", body, &out)
	return out.ID, err
}

func (c *Client) Start(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", "", nil, nil)
}

// Remove kills and removes a container with its anonymous volumes. One
// already gone counts as removed.
func (c *Client) Remove(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=1&v=1", "", nil, nil)
	if NotFound(err) {
		return nil
	}
	return err
}

// Wait blocks until the container stops and returns its exit code.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int `json:"StatusCode"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/wait", "", nil, &out)
	return out.StatusCode, err
}

// Logs returns a TTY container's output.
func (c *Client) Logs(ctx context.Context, id string) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs?stdout=1&stderr=1", "", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

// Pid returns the host pid of a running container's first process.
func (c *Client) Pid(ctx context.Context, id string) (int, error) {
	var out struct {
		State struct {
			Pid int `json:"Pid"`
		} `json:"State"`
	}
	err := c.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", "", nil, &out)
	return out.State.Pid, err
}

type Network struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Options map[string]string `json:"Options"`
	IPAM    struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// Network returns a network, or false when there is none by that name.
func (c *Client) Network(ctx context.Context, name string) (Network, bool, error) {
	var out Network
	err := c.call(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), "", nil, &out)
	if NotFound(err) {
		return Network{}, false, nil
	}
	return out, err == nil, err
}

// NetworkSpec is a bridge network with its own bridge name.
type NetworkSpec struct {
	Name   string
	Bridge string
	// ICC allows traffic between containers on the network.
	ICC    bool
	Labels map[string]string
}

func (c *Client) CreateNetwork(ctx context.Context, s NetworkSpec) error {
	body := map[string]any{
		"Name":   s.Name,
		"Driver": "bridge",
		"Options": map[string]string{
			"com.docker.network.bridge.name":       s.Bridge,
			"com.docker.network.bridge.enable_icc": strconv.FormatBool(s.ICC),
		},
		"Labels": s.Labels,
	}
	return c.call(ctx, http.MethodPost, "/networks/create", "application/json", body, nil)
}

func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	err := c.call(ctx, http.MethodDelete, "/networks/"+url.PathEscape(name), "", nil, nil)
	if NotFound(err) {
		return nil
	}
	return err
}

// ImageLabels returns an image's labels, or false when the image is missing.
func (c *Client) ImageLabels(ctx context.Context, ref string) (map[string]string, bool, error) {
	img, ok, err := c.Image(ctx, ref)
	return img.Labels, ok, err
}

// ImageInfo is what boxrunner reads of an image.
type ImageInfo struct {
	ID     string
	Labels map[string]string
}

// Image inspects an image; ok is false when there is none.
func (c *Client) Image(ctx context.Context, ref string) (ImageInfo, bool, error) {
	var out struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	err := c.call(ctx, http.MethodGet, "/images/"+ref+"/json", "", nil, &out)
	if NotFound(err) {
		return ImageInfo{}, false, nil
	}
	return ImageInfo{ID: out.ID, Labels: out.Config.Labels}, err == nil, err
}

func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	err := c.call(ctx, http.MethodDelete, "/images/"+ref, "", nil, nil)
	if NotFound(err) {
		return nil
	}
	return err
}

// BuildOptions names the image a build produces and how it is built.
type BuildOptions struct {
	Tag    string
	Args   map[string]string
	Labels map[string]string
	// Pull fetches the base image fresh. Off for images built FROM a local one.
	Pull bool
	// Dockerfile is the file's path inside the context; empty means "Dockerfile".
	Dockerfile string
}

// Build builds a tar build context into an image. It returns the first error
// the build reports.
func (c *Client) Build(ctx context.Context, tarContext io.Reader, o BuildOptions) error {
	q := url.Values{"t": {o.Tag}, "rm": {"1"}, "forcerm": {"1"}}
	if len(o.Args) > 0 {
		b, _ := json.Marshal(o.Args)
		q.Set("buildargs", string(b))
	}
	if len(o.Labels) > 0 {
		b, _ := json.Marshal(o.Labels)
		q.Set("labels", string(b))
	}
	if o.Pull {
		q.Set("pull", "1")
	}
	if o.Dockerfile != "" {
		q.Set("dockerfile", o.Dockerfile)
	}
	resp, err := c.do(ctx, http.MethodPost, "/build?"+q.Encode(), "application/x-tar", tarContext)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(bufio.NewReader(resp.Body))
	for {
		var line struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&line); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("docker build: %w", err)
		}
		if line.Error != "" {
			return fmt.Errorf("docker build: %s", line.Error)
		}
	}
}

func (c *Client) call(ctx context.Context, method, path, contentType string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.do(ctx, method, path, contentType, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		defer resp.Body.Close()
		var e struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return nil, &Error{Status: resp.StatusCode, Message: e.Message}
	}
	return resp, nil
}
