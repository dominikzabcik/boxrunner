// Package mirror runs a pull-through cache of Docker Hub on the box. The
// Docker daemons inside jobs use it as their registry mirror, so an image one
// job pulled comes from the box for the next.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"

	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/layout"
)

// Image is registry 2.8.3, pinned to its multi-architecture index.
const Image = "registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"

// Addr is where the mirror listens: the job network's gateway, which only the
// host and the containers on that network can reach.
func Addr(gateway netip.Addr) netip.AddrPort {
	return netip.AddrPortFrom(gateway, layout.MirrorPort)
}

// Find returns the mirror's address on the box's job network.
func Find(ctx context.Context, d *docker.Client) (netip.AddrPort, error) {
	gateway, err := d.Gateway(ctx, layout.Network)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("the registry mirror: %w", err)
	}
	return Addr(gateway), nil
}

// URL is the mirror as the Docker daemon in a job names it.
func URL(addr netip.AddrPort) string { return "http://" + addr.String() }

// Identity is what the mirror's container label holds, so a new image pin or
// a new address replaces the container.
func Identity(addr netip.AddrPort) string { return Image + " " + addr.String() }

// State is what the box has of the mirror.
type State struct {
	// Identity is the container's label; "" when there is no container.
	Identity string
	Running  bool
	// Volume is whether the cache volume exists.
	Volume bool
}

// Current is the state of a mirror that serves at addr.
func Current(addr netip.AddrPort) State {
	return State{Identity: Identity(addr), Running: true, Volume: true}
}

// Observe reads the mirror's container and volume.
func Observe(ctx context.Context, d *docker.Client) (State, error) {
	var s State
	list, err := d.List(ctx, layout.LabelMirror, false)
	if err != nil {
		return s, err
	}
	for _, c := range list {
		s.Identity, s.Running = c.Labels[layout.LabelMirror], c.Running()
	}
	s.Volume, err = d.Volume(ctx, layout.Mirror)
	return s, err
}

// Spec is the mirror's container: Docker Hub in proxy mode, on the host's
// network but listening on addr alone, its cache in a named volume, restarted
// with Docker.
func Spec(addr netip.AddrPort) docker.ContainerSpec {
	return docker.ContainerSpec{
		Name:  layout.Mirror,
		Image: Image,
		Env: []string{
			"REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io",
			"REGISTRY_HTTP_ADDR=" + addr.String(),
		},
		Labels:  map[string]string{layout.LabelMirror: Identity(addr)},
		Network: "host",
		Restart: "unless-stopped",
		Volumes: map[string]string{layout.Mirror: "/var/lib/registry"},
	}
}

// Ensure leaves a running mirror at addr, replacing a container that is
// stopped or was made for another image or address. The cache volume stays.
func Ensure(ctx context.Context, d *docker.Client, addr netip.AddrPort) error {
	s, err := Observe(ctx, d)
	if err != nil || s == Current(addr) {
		return err
	}
	if err := d.Remove(ctx, layout.Mirror); err != nil {
		return err
	}
	if _, ok, err := d.Image(ctx, Image); err != nil {
		return err
	} else if !ok {
		if err := d.Pull(ctx, Image); err != nil {
			return err
		}
	}
	if err := d.CreateVolume(ctx, layout.Mirror, map[string]string{layout.LabelMirror: "cache"}); err != nil {
		return err
	}
	id, err := d.Create(ctx, Spec(addr))
	if err != nil {
		return err
	}
	return d.Start(ctx, id)
}

// Remove deletes the mirror's container, its cache and its image, unless
// another container uses that image.
func Remove(ctx context.Context, d *docker.Client) error {
	if err := d.Remove(ctx, layout.Mirror); err != nil {
		return err
	}
	if err := d.RemoveVolume(ctx, layout.Mirror); err != nil {
		return err
	}
	var inUse *docker.Error
	if err := d.RemoveImage(ctx, Image); err != nil && !(errors.As(err, &inUse) && inUse.Status == http.StatusConflict) {
		return err
	}
	return nil
}
