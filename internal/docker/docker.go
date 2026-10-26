// Package docker is a small client for the few Docker Engine API calls the
// agent needs, over the unix socket.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
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
	// Runtimes are the OCI runtimes a container can name, by name.
	Runtimes            map[string]struct{} `json:"Runtimes"`
	DefaultAddressPools []AddressPool       `json:"DefaultAddressPools"`
	LiveRestore         bool                `json:"LiveRestoreEnabled"`
}

// AddressPool is a range Docker carves new networks' subnets from.
type AddressPool struct {
	Base string `json:"Base"`
	Size int    `json:"Size"`
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
	ImageID string            `json:"ImageID"`
}

// UsedImages returns the IDs of the images containers use, leaving out the
// containers that carry one of the labels (keys), such as the ones uninstall
// is about to remove.
func UsedImages(containers []Container, except ...string) map[string]bool {
	used := map[string]bool{}
	for _, c := range containers {
		if !slices.ContainsFunc(except, func(key string) bool { _, ok := c.Labels[key]; return ok }) {
			used[c.ImageID] = true
		}
	}
	return used
}

func (c Container) Running() bool { return c.State == "running" }

func (c Container) CreatedAt() time.Time { return time.Unix(c.Created, 0).UTC() }

// List returns every container, running or not, that carries the label
// (key or key=value), or every container when label is empty. With sizes,
// Docker also measures what each one wrote, which walks its files.
func (c *Client) List(ctx context.Context, label string, sizes bool) ([]Container, error) {
	q := url.Values{"all": {"1"}}
	if label != "" {
		filters, _ := json.Marshal(map[string][]string{"label": {label}})
		q.Set("filters", string(filters))
	}
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
	// Runtime is the OCI runtime; empty means Docker's default.
	Runtime string
	Tty     bool
	// Restart is the restart policy, such as unless-stopped; empty means none.
	Restart string
	// Volumes mounts named volumes: volume name to path in the container.
	Volumes map[string]string
	// DNS are the resolvers behind the container's own: Docker's resolver
	// forwards what it does not know to them, from the container's address.
	DNS []string
}

type restartPolicy struct {
	Name string `json:"Name"`
}

type mount struct {
	Type   string `json:"Type"`
	Source string `json:"Source"`
	Target string `json:"Target"`
}

type hostConfig struct {
	NanoCpus      int64          `json:"NanoCpus,omitempty"`
	Memory        int64          `json:"Memory,omitempty"`
	MemorySwap    int64          `json:"MemorySwap,omitempty"`
	PidsLimit     int64          `json:"PidsLimit,omitempty"`
	ShmSize       int64          `json:"ShmSize,omitempty"`
	CgroupParent  string         `json:"CgroupParent,omitempty"`
	NetworkMode   string         `json:"NetworkMode,omitempty"`
	Runtime       string         `json:"Runtime,omitempty"`
	RestartPolicy *restartPolicy `json:"RestartPolicy,omitempty"`
	Mounts        []mount        `json:"Mounts,omitempty"`
	Dns           []string       `json:"Dns,omitempty"`
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
			ShmSize: s.ShmSize, CgroupParent: s.CgroupParent, NetworkMode: s.Network, Runtime: s.Runtime, Dns: s.DNS,
		},
	}
	if s.Restart != "" {
		body.HostConfig.RestartPolicy = &restartPolicy{s.Restart}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Volumes)) {
		body.HostConfig.Mounts = append(body.HostConfig.Mounts, mount{"volume", name, s.Volumes[name]})
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

// Stop sends the container SIGTERM and kills it after grace.
func (c *Client) Stop(ctx context.Context, id string, grace time.Duration) error {
	err := c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop?t="+strconv.Itoa(int(grace.Seconds())), "", nil, nil)
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
	return c.LogsSince(ctx, id, time.Time{})
}

// LogsSince returns a container's output from a moment on, up to 1 MiB.
// Without a TTY, every line carries Docker's 8-byte stream header.
func (c *Client) LogsSince(ctx context.Context, id string, since time.Time) (string, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	if !since.IsZero() {
		q.Set("since", fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()))
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs?"+q.Encode(), "", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

// Tail returns the last lines a container without a TTY wrote.
func (c *Client) Tail(ctx context.Context, id string, lines int) ([]string, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(lines)}}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs?"+q.Encode(), "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	r := bufio.NewReader(io.LimitReader(resp.Body, 1<<20))
	header := make([]byte, 8)
	// Each frame is a stream byte, three zero bytes and the payload's length.
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			break
		}
		if _, err := io.CopyN(&out, r, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			break
		}
	}
	return strings.Split(strings.TrimRight(out.String(), "\n"), "\n"), nil
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
	Labels  map[string]string `json:"Labels"`
	IPAM    struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	// Containers are the endpoints on the network, by container id.
	Containers map[string]Endpoint `json:"Containers"`
}

// Endpoint is a container's place on a network.
type Endpoint struct {
	Name string `json:"Name"`
	// IPv4Address is the container's address with its prefix length.
	IPv4Address string `json:"IPv4Address"`
}

// Addr is the container's IPv4 address, if it has one.
func (e Endpoint) Addr() (netip.Addr, bool) {
	prefix, err := netip.ParsePrefix(e.IPv4Address)
	return prefix.Addr(), err == nil
}

// Subnet is the network's IPv4 subnet.
func (n Network) Subnet() (netip.Prefix, bool) {
	for _, c := range n.IPAM.Config {
		if p, err := netip.ParsePrefix(c.Subnet); err == nil && p.Addr().Is4() {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// Networks lists the networks that carry a label (key or key=value), or every
// network when label is empty.
func (c *Client) Networks(ctx context.Context, label string) ([]Network, error) {
	q := url.Values{}
	if label != "" {
		filters, _ := json.Marshal(map[string][]string{"label": {label}})
		q.Set("filters", string(filters))
	}
	var list []struct {
		Name string `json:"Name"`
	}
	if err := c.call(ctx, http.MethodGet, "/networks?"+q.Encode(), "", nil, &list); err != nil {
		return nil, err
	}
	// The listing leaves out the endpoints, so each network is read again.
	var out []Network
	for _, l := range list {
		n, ok, err := c.Network(ctx, l.Name)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// Gateway is the network's IPv4 gateway: the host's address on its bridge.
func (n Network) Gateway() (netip.Addr, bool) {
	for _, c := range n.IPAM.Config {
		if a, err := netip.ParseAddr(c.Gateway); err == nil && a.Is4() {
			return a, true
		}
	}
	return netip.Addr{}, false
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

// Gateway returns a network's IPv4 gateway, the host's address on it.
func (c *Client) Gateway(ctx context.Context, name string) (netip.Addr, error) {
	n, ok, err := c.Network(ctx, name)
	if err != nil {
		return netip.Addr{}, err
	}
	gateway, found := n.Gateway()
	if !ok || !found {
		return netip.Addr{}, fmt.Errorf("the Docker network %s has no IPv4 gateway", name)
	}
	return gateway, nil
}

// NetworkSpec is a bridge network with its own bridge name.
type NetworkSpec struct {
	Name   string
	Bridge string
	// ICC allows traffic between containers on the network.
	ICC    bool
	Labels map[string]string
	// Subnet and Gateway, when valid, are the network's; otherwise Docker
	// takes a subnet from its address pools.
	Subnet  netip.Prefix
	Gateway netip.Addr
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
	if s.Subnet.IsValid() && s.Gateway.IsValid() {
		body["IPAM"] = map[string]any{"Config": []map[string]string{{"Subnet": s.Subnet.String(), "Gateway": s.Gateway.String()}}}
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

// CreateVolume creates a named volume, or keeps the one by that name.
func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.call(ctx, http.MethodPost, "/volumes/create", "application/json", map[string]any{"Name": name, "Labels": labels}, nil)
}

// Volume reports whether a named volume exists.
func (c *Client) Volume(ctx context.Context, name string) (bool, error) {
	err := c.call(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), "", nil, nil)
	if NotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// RemoveVolume removes a named volume and its data. One already gone counts
// as removed.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), "", nil, nil)
	if NotFound(err) {
		return nil
	}
	return err
}

// Pull fetches an image, such as name@sha256:…, from its registry.
func (c *Client) Pull(ctx context.Context, ref string) error {
	resp, err := c.do(ctx, http.MethodPost, "/images/create?"+url.Values{"fromImage": {ref}}.Encode(), "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return streamError(resp.Body, "docker pull")
}

// Tag gives the image ref, such as name@sha256:…, the tag repo:tag.
func (c *Client) Tag(ctx context.Context, ref, repo, tag string) error {
	return c.call(ctx, http.MethodPost, "/images/"+ref+"/tag?"+url.Values{"repo": {repo}, "tag": {tag}}.Encode(), "", nil, nil)
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

// ImageSummary is one image as Docker lists it. Size counts the layers of
// the image it was built from too.
type ImageSummary struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	// RepoDigests are the references name@sha256:… the image was pulled by.
	// A pull by digest sets no tag, and an image goes only when its last
	// reference does, tag or digest.
	RepoDigests []string          `json:"RepoDigests"`
	Labels      map[string]string `json:"Labels"`
	Size        int64             `json:"Size"`
}

// Images lists the images that are not intermediate layers of a build.
func (c *Client) Images(ctx context.Context) ([]ImageSummary, error) {
	var out []ImageSummary
	err := c.call(ctx, http.MethodGet, "/images/json", "", nil, &out)
	return out, err
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
	return streamError(resp.Body, "docker build")
}

// streamError reads a JSON progress stream to its end and returns the first
// error it reports.
func streamError(r io.Reader, what string) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	for {
		var line struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&line); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if line.Error != "" {
			return fmt.Errorf("%s: %s", what, line.Error)
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
