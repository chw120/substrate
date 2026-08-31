// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package deviceplugin advertises host device nodes (for example /dev/kvm) to
// kubelet as extended resources, so a worker pod can be granted just those
// devices instead of running privileged.
//
// Device access is gated by the cgroup v2 device controller, which denies by
// default before DAC is consulted: no capability, hostPath mount, or
// supplemental group grants it. Kubelet's device manager adds the narrow allow
// rule, emitting the device node and a matching cgroup allow for each device.
package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// sharedDeviceCount is how many allocatable units of a shared device are
// advertised. A shared device is a pseudo-device any number of containers may
// hold at once, so the count means nothing except as an exhaustion limit; keep
// it far above any max-pods-per-node setting so device accounting never
// constrains scheduling. An exclusive device advertises one unit per node
// instead, because there the count is the real supply.
const sharedDeviceCount = 4096

// registerTimeout bounds a single registration attempt against kubelet.
const registerTimeout = 30 * time.Second

// Extended resource names advertised by atelet and requested by worker pods.
// Both sides import these so the strings cannot drift apart.
const (
	// ResourceKVM grants /dev/kvm, which the micro-VM runtime needs to create a
	// VM (cloud-hypervisor fails with EPERM on VmCreate without it).
	ResourceKVM = "ate.dev/kvm"
	// ResourceLoop grants one loop device, which a micro-VM worker needs to
	// mount a durable-dir erofs image. Unlike /dev/kvm this is an exclusive
	// grant: a loop device backs exactly one file, so two workers handed the
	// same one would fight over it.
	ResourceLoop = "ate.dev/loop"
)

// loopDeviceGlob matches the node's loop device nodes and nothing else. The
// digit is what keeps /dev/loop-control out: nothing needs it here, because a
// worker mounts the device it was granted rather than asking the kernel for a
// free one.
const loopDeviceGlob = "loop[0-9]*"

// SandboxDevices returns the host devices a sandbox runtime needs a grant for,
// as they appear under devRoot. atelet advertises whichever of these exist on
// its node.
//
// Only devices the container runtime denies by default belong here. The
// micro-VM runtime also opens /dev/net/tun, but that is in the runtime's
// default allow-list, so the worker gets it as an ordinary bind mount instead.
//
// The loop pool is discovered rather than declared: how many nodes exist is the
// loop module's max_loop, which differs between node images, and advertising a
// node that is not there would let the scheduler place a worker that then
// cannot mount anything.
func SandboxDevices(devRoot string) []HostDevice {
	devs := []HostDevice{
		{ResourceName: ResourceKVM, Nodes: []string{"/dev/kvm"}, Shared: true},
	}
	if loops := discoverLoopDevices(devRoot); len(loops) > 0 {
		devs = append(devs, HostDevice{ResourceName: ResourceLoop, Nodes: loops})
	}
	return devs
}

// discoverLoopDevices lists the node's loop devices as host paths, sorted, by
// globbing devRoot. A node whose loop module never loaded has none, and then
// the resource is simply not advertised.
func discoverLoopDevices(devRoot string) []string {
	matches, err := filepath.Glob(filepath.Join(devRoot, loopDeviceGlob))
	if err != nil {
		// The only error Glob reports is a malformed pattern, which is a
		// constant here.
		return nil
	}
	sort.Strings(matches)
	nodes := make([]string, 0, len(matches))
	for _, m := range matches {
		nodes = append(nodes, "/dev/"+filepath.Base(m))
	}
	return nodes
}

// HostDevice is one extended resource and the device node or nodes behind it.
type HostDevice struct {
	// ResourceName is the fully-qualified extended resource name pods request,
	// e.g. "ate.dev/kvm".
	ResourceName string
	// Nodes are the device nodes this resource covers, e.g. ["/dev/kvm"] or
	// every /dev/loopN on the node. Each is exposed to the container at its own
	// host path.
	Nodes []string
	// Shared marks a device many containers may hold at once. A shared resource
	// has exactly one node and hands it to every claimant; an exclusive one
	// hands out its nodes individually, so kubelet's accounting is what stops
	// two workers getting the same device.
	Shared bool
}

// Present reports whether at least one of the device's nodes exists on the node
// as a device (character or block). devRoot is where the node's /dev is mounted
// for inspection, our own container having a minimal /dev of its own; Allocate
// still reports the real host paths, which kubelet resolves on the node.
func (d HostDevice) Present(devRoot string) bool {
	return len(d.present(devRoot)) > 0
}

// present returns the subset of Nodes that exist under devRoot as device nodes.
func (d HostDevice) present(devRoot string) []string {
	out := make([]string, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		fi, err := os.Stat(resolve(devRoot, n))
		if err == nil && fi.Mode()&os.ModeDevice != 0 {
			out = append(out, n)
		}
	}
	return out
}

// resolve maps a device's host path into devRoot ("/dev/kvm" ->
// "<devRoot>/kvm").
func resolve(devRoot, node string) string {
	return filepath.Join(devRoot, strings.TrimPrefix(node, "/dev/"))
}

// Available returns devs narrowed to what is actually present on this node:
// resources with no node left are dropped, and the rest keep only the nodes
// that exist. atelet runs on every node, so advertising a resource only where
// its device exists keeps pods requesting it off nodes that cannot run them.
func Available(devs []HostDevice, devRoot string) []HostDevice {
	out := make([]HostDevice, 0, len(devs))
	for _, d := range devs {
		if nodes := d.present(devRoot); len(nodes) > 0 {
			d.Nodes = nodes
			out = append(out, d)
		}
	}
	return out
}

// Plugin serves the kubelet device plugin API for a single HostDevice.
type Plugin struct {
	pluginapi.UnimplementedDevicePluginServer

	dev     HostDevice
	socket  string
	devices []*pluginapi.Device
	// nodeByID maps an advertised device ID to the host path Allocate hands
	// back. Empty for a shared device, where every ID means the same node.
	nodeByID map[string]string
}

var _ pluginapi.DevicePluginServer = (*Plugin)(nil)

// New builds a Plugin for dev. Call Run to serve it.
func New(dev HostDevice) *Plugin {
	p := &Plugin{
		dev: dev,
		// One socket per resource, in the directory kubelet watches. The name is
		// derived from the resource so two plugins never collide.
		socket: filepath.Join(pluginapi.DevicePluginPath, socketName(dev.ResourceName)),
	}
	if dev.Shared {
		// Interchangeable units of one node: the ID exists only so kubelet has
		// something to count.
		base := filepath.Base(dev.Nodes[0])
		p.devices = make([]*pluginapi.Device, 0, sharedDeviceCount)
		for i := range sharedDeviceCount {
			p.devices = append(p.devices, &pluginapi.Device{
				ID:     fmt.Sprintf("%s-%d", base, i),
				Health: pluginapi.Healthy,
			})
		}
		return p
	}
	// One unit per node, named after it, so the ID kubelet allocates is what
	// says which node the container gets.
	p.devices = make([]*pluginapi.Device, 0, len(dev.Nodes))
	p.nodeByID = make(map[string]string, len(dev.Nodes))
	for _, node := range dev.Nodes {
		id := filepath.Base(node)
		p.devices = append(p.devices, &pluginapi.Device{ID: id, Health: pluginapi.Healthy})
		p.nodeByID[id] = node
	}
	return p
}

// socketName maps a resource name to a socket filename ("ate.dev/kvm" ->
// "ate.dev-kvm.sock").
func socketName(resourceName string) string {
	return filepath.Base(filepath.Dir(resourceName)) + "-" + filepath.Base(resourceName) + ".sock"
}

// Run serves the plugin and keeps it registered until ctx is cancelled. Kubelet
// forgets registered plugins when it restarts and signals that by recreating its
// socket, so re-register then or the resource disappears from the node.
func (p *Plugin) Run(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("while creating fsnotify watcher: %w", err)
	}
	defer watcher.Close()
	if err := watcher.Add(pluginapi.DevicePluginPath); err != nil {
		return fmt.Errorf("while watching %q: %w", pluginapi.DevicePluginPath, err)
	}

	for ctx.Err() == nil {
		srv, err := p.serveAndRegister(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "Device plugin registration failed; retrying",
				slog.String("resource", p.dev.ResourceName), slog.Any("err", err))
			if !sleepCtx(ctx, 5*time.Second) {
				break
			}
			continue
		}
		slog.InfoContext(ctx, "Device plugin registered",
			slog.String("resource", p.dev.ResourceName),
			slog.String("devices", strings.Join(p.dev.Nodes, ",")))

		p.waitForKubeletRestart(ctx, watcher)
		srv.Stop()
	}
	return ctx.Err()
}

// serveAndRegister starts the gRPC server on the plugin socket and registers it
// with kubelet. The returned server is stopped by the caller.
func (p *Plugin) serveAndRegister(ctx context.Context) (*grpc.Server, error) {
	// A leftover socket from a previous run would make Listen fail with EADDRINUSE.
	if err := os.Remove(p.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("while removing stale socket %q: %w", p.socket, err)
	}
	lis, err := net.Listen("unix", p.socket)
	if err != nil {
		return nil, fmt.Errorf("while listening on %q: %w", p.socket, err)
	}

	srv := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(srv, p)
	go func() {
		if err := srv.Serve(lis); err != nil {
			slog.ErrorContext(ctx, "Device plugin server stopped",
				slog.String("resource", p.dev.ResourceName), slog.Any("err", err))
		}
	}()

	if err := p.register(ctx); err != nil {
		srv.Stop()
		return nil, err
	}
	return srv, nil
}

// register tells kubelet which resource this plugin's socket serves.
func (p *Plugin) register(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()

	conn, err := grpc.NewClient("unix://"+pluginapi.KubeletSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("while connecting to kubelet: %w", err)
	}
	defer conn.Close()

	_, err = pluginapi.NewRegistrationClient(conn).Register(ctx, &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     filepath.Base(p.socket),
		ResourceName: p.dev.ResourceName,
	})
	if err != nil {
		return fmt.Errorf("while registering %q with kubelet: %w", p.dev.ResourceName, err)
	}
	return nil
}

// waitForKubeletRestart blocks until kubelet recreates its socket (meaning it
// restarted and dropped our registration) or ctx is cancelled.
func (p *Plugin) waitForKubeletRestart(ctx context.Context, watcher *fsnotify.Watcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Name == pluginapi.KubeletSocket && event.Has(fsnotify.Create) {
				slog.InfoContext(ctx, "Kubelet restarted; re-registering device plugin",
					slog.String("resource", p.dev.ResourceName))
				return
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.ErrorContext(ctx, "Device plugin socket watch error",
				slog.String("resource", p.dev.ResourceName), slog.Any("err", err))
		}
	}
}

// GetDevicePluginOptions implements the device plugin API; the defaults are
// correct here.
func (p *Plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch streams the device list to kubelet. The set is static, so send it
// once and hold the stream open until shutdown.
func (p *Plugin) ListAndWatch(_ *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: p.devices}); err != nil {
		return fmt.Errorf("while sending device list: %w", err)
	}
	<-stream.Context().Done()
	return nil
}

// Allocate returns the device nodes for each requested container. Kubelet turns
// each DeviceSpec into a device node plus a matching cgroup allow, so the
// container gets those devices and no others.
//
// A shared device ignores the requested IDs, which are interchangeable names
// for one node. An exclusive device must honor them: the ID is the only thing
// that says which node kubelet reserved for this container, and handing back a
// different one would give two containers the same device.
func (p *Plugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	resp := &pluginapi.AllocateResponse{
		ContainerResponses: make([]*pluginapi.ContainerAllocateResponse, 0, len(req.GetContainerRequests())),
	}
	for _, cr := range req.GetContainerRequests() {
		nodes, err := p.nodesFor(cr.GetDevicesIds())
		if err != nil {
			return nil, err
		}
		specs := make([]*pluginapi.DeviceSpec, 0, len(nodes))
		for _, node := range nodes {
			specs = append(specs, &pluginapi.DeviceSpec{
				HostPath:      node,
				ContainerPath: node,
				// Read/write, but not mknod: the container is handed these
				// nodes, not the ability to mint new ones.
				Permissions: "rw",
			})
		}
		resp.ContainerResponses = append(resp.ContainerResponses, &pluginapi.ContainerAllocateResponse{Devices: specs})
	}
	return resp, nil
}

// nodesFor maps the IDs kubelet allocated to the host paths to expose.
func (p *Plugin) nodesFor(ids []string) ([]string, error) {
	if p.dev.Shared {
		return p.dev.Nodes[:1], nil
	}
	nodes := make([]string, 0, len(ids))
	for _, id := range ids {
		node, ok := p.nodeByID[id]
		if !ok {
			return nil, fmt.Errorf("%s: no device node for allocated id %q", p.dev.ResourceName, id)
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// sleepCtx waits for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
