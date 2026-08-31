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

package deviceplugin

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/grpc"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// sharedKVM is the shared-device shape every test that is not about exclusive
// devices uses.
func sharedKVM() HostDevice {
	return HostDevice{ResourceName: "ate.dev/kvm", Nodes: []string{"/dev/kvm"}, Shared: true}
}

// Each resource gets its own socket in the directory kubelet watches, and two
// resources must never collide on one filename.
func TestSocketPathsAreDistinctAndInKubeletDir(t *testing.T) {
	kvm := New(sharedKVM())
	other := New(HostDevice{ResourceName: "ate.dev/other", Nodes: []string{"/dev/other"}, Shared: true})

	if kvm.socket == other.socket {
		t.Errorf("sockets collide: %q", kvm.socket)
	}
	for _, p := range []*Plugin{kvm, other} {
		if got := filepath.Dir(p.socket) + "/"; got != pluginapi.DevicePluginPath {
			t.Errorf("socket %q is not in %q", p.socket, pluginapi.DevicePluginPath)
		}
		if filepath.Ext(p.socket) != ".sock" {
			t.Errorf("socket %q should end in .sock", p.socket)
		}
	}
}

// The advertised count for a shared device must stay far above any
// max-pods-per-node setting, or kubelet would report the resource exhausted and
// stop scheduling workers.
func TestSharedDeviceCountExceedsMaxPodsPerNode(t *testing.T) {
	// The largest supported max-pods values are in the low hundreds.
	const highestRealisticMaxPods = 256
	if sharedDeviceCount <= highestRealisticMaxPods {
		t.Errorf("sharedDeviceCount = %d, want well above %d", sharedDeviceCount, highestRealisticMaxPods)
	}
	p := New(sharedKVM())
	if len(p.devices) != sharedDeviceCount {
		t.Errorf("advertised %d devices, want %d", len(p.devices), sharedDeviceCount)
	}
	seen := make(map[string]bool, len(p.devices))
	for _, d := range p.devices {
		if seen[d.ID] {
			t.Fatalf("duplicate device ID %q", d.ID)
		}
		seen[d.ID] = true
		if d.Health != pluginapi.Healthy {
			t.Errorf("device %q health = %q, want %q", d.ID, d.Health, pluginapi.Healthy)
		}
	}
}

// Allocate must hand back exactly the one device node, read/write but not
// mknod, once per requested container.
func TestAllocateReturnsOnlyTheRequestedDevice(t *testing.T) {
	p := New(sharedKVM())

	resp, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"kvm-0"}},
			{DevicesIds: []string{"kvm-1"}},
		},
	})
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}
	if got := len(resp.GetContainerResponses()); got != 2 {
		t.Fatalf("got %d container responses, want 2", got)
	}
	for i, cr := range resp.GetContainerResponses() {
		devs := cr.GetDevices()
		if len(devs) != 1 {
			t.Fatalf("container %d: got %d devices, want 1", i, len(devs))
		}
		d := devs[0]
		if d.GetHostPath() != "/dev/kvm" || d.GetContainerPath() != "/dev/kvm" {
			t.Errorf("container %d: paths = %q -> %q, want /dev/kvm both", i, d.GetHostPath(), d.GetContainerPath())
		}
		if d.GetPermissions() != "rw" {
			t.Errorf("container %d: permissions = %q, want rw (no mknod)", i, d.GetPermissions())
		}
	}
}

// A node without the device must not advertise the resource, so the scheduler
// keeps pods that need it off that node.
func TestAvailableFiltersToPresentDevices(t *testing.T) {
	// /dev/null is a character device everywhere the tests run; resolving it
	// under "/" mirrors how the node's /dev is mounted at a different root.
	devs := []HostDevice{
		{ResourceName: "ate.dev/null", Nodes: []string{"/dev/null"}, Shared: true},
		{ResourceName: "ate.dev/absent", Nodes: []string{"/dev/definitely-not-a-device"}},
	}
	got := Available(devs, "/dev")
	if len(got) != 1 || got[0].ResourceName != "ate.dev/null" {
		t.Fatalf("Available() = %+v, want only ate.dev/null", got)
	}
}

// A multi-node resource is kept but narrowed to the nodes that exist. Handing
// kubelet a node that is not there would let it allocate a device the worker
// then cannot open, which fails at mount time rather than at scheduling time.
func TestAvailableNarrowsToPresentNodes(t *testing.T) {
	devs := []HostDevice{{
		ResourceName: ResourceLoop,
		Nodes:        []string{"/dev/null", "/dev/definitely-not-a-device", "/dev/zero"},
	}}
	got := Available(devs, "/dev")
	if len(got) != 1 {
		t.Fatalf("Available() = %+v, want one resource", got)
	}
	want := []string{"/dev/null", "/dev/zero"}
	if !slices.Equal(got[0].Nodes, want) {
		t.Errorf("Nodes = %v, want %v", got[0].Nodes, want)
	}
}

// A regular file at the device path is not a device; advertising it would grant
// a meaningless resource.
func TestPresentRejectsNonDevice(t *testing.T) {
	regular := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if sharedKVM().Present(filepath.Dir(regular)) {
		t.Errorf("Present() = true for a regular file at %q", regular)
	}
}

// Loop devices are BLOCK devices, unlike /dev/kvm. A presence check that only
// accepted character devices would silently never advertise the loop pool, and
// the erofs durable-dir opt-in would leave workers unschedulable.
func TestPresentAcceptsBlockDevices(t *testing.T) {
	// /dev/loop0 is not guaranteed to exist wherever the tests run, so assert
	// the rule against whichever block device the machine does have.
	entries, err := os.ReadDir("/dev")
	if err != nil {
		t.Skipf("cannot read /dev: %v", err)
	}
	var block string
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeDevice != 0 && fi.Mode()&os.ModeCharDevice == 0 {
			block = e.Name()
			break
		}
	}
	if block == "" {
		t.Skip("no block device under /dev on this machine")
	}
	d := HostDevice{ResourceName: ResourceLoop, Nodes: []string{"/dev/" + block}}
	if !d.Present("/dev") {
		t.Errorf("Present() = false for block device /dev/%s", block)
	}
}

// An exclusive resource advertises one unit per node, named after it, and
// Allocate returns the node kubelet actually reserved. Returning any other one
// would hand two workers the same loop device, and the second mount fails with
// the first worker's image still attached.
func TestExclusiveAllocateHonorsRequestedIDs(t *testing.T) {
	p := New(HostDevice{
		ResourceName: ResourceLoop,
		Nodes:        []string{"/dev/loop0", "/dev/loop1", "/dev/loop2"},
	})
	if len(p.devices) != 3 {
		t.Fatalf("advertised %d devices, want one per node", len(p.devices))
	}
	if got := p.devices[1].ID; got != "loop1" {
		t.Errorf("device[1].ID = %q, want loop1", got)
	}

	resp, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"loop2"}}},
	})
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}
	devs := resp.GetContainerResponses()[0].GetDevices()
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1", len(devs))
	}
	if devs[0].GetHostPath() != "/dev/loop2" || devs[0].GetContainerPath() != "/dev/loop2" {
		t.Errorf("paths = %q -> %q, want /dev/loop2 both",
			devs[0].GetHostPath(), devs[0].GetContainerPath())
	}
	if devs[0].GetPermissions() != "rw" {
		t.Errorf("permissions = %q, want rw (no mknod)", devs[0].GetPermissions())
	}
}

// An ID we never advertised means kubelet and this plugin disagree about the
// device set. Guessing a node would be worse than failing the allocation: the
// worker would get a device somebody else holds.
func TestExclusiveAllocateRejectsUnknownID(t *testing.T) {
	p := New(HostDevice{ResourceName: ResourceLoop, Nodes: []string{"/dev/loop0"}})
	if _, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"loop9"}}},
	}); err == nil {
		t.Error("Allocate() with an unadvertised ID = nil error, want failure")
	}
}

// The loop pool is discovered from the node's /dev, and /dev/loop-control must
// not be in it: it is a character device that hands out loop devices, which is
// exactly the privilege the grant exists to avoid.
func TestDiscoverLoopDevicesExcludesLoopControl(t *testing.T) {
	devRoot := t.TempDir()
	for _, name := range []string{"loop0", "loop1", "loop10", "loop-control", "kvm", "null"} {
		if err := os.WriteFile(filepath.Join(devRoot, name), nil, 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	got := discoverLoopDevices(devRoot)
	want := []string{"/dev/loop0", "/dev/loop1", "/dev/loop10"}
	if !slices.Equal(got, want) {
		t.Errorf("discoverLoopDevices() = %v, want %v", got, want)
	}
}

// A node whose loop module never loaded advertises no loop resource at all,
// rather than an empty one: an empty resource would still let the scheduler
// place a worker that then cannot mount anything.
func TestSandboxDevicesOmitsLoopWhenNoneExist(t *testing.T) {
	devRoot := t.TempDir()
	for _, d := range SandboxDevices(devRoot) {
		if d.ResourceName == ResourceLoop {
			t.Errorf("advertised %s with no loop device present", ResourceLoop)
		}
		if d.ResourceName == ResourceKVM && !d.Shared {
			t.Errorf("%s must be shared", ResourceKVM)
		}
	}
}

// ListAndWatch sends the full device list, then holds the stream open until the
// stream context is cancelled (kubelet keeps it open for the plugin's lifetime).
func TestListAndWatchSendsDevicesThenBlocks(t *testing.T) {
	p := New(sharedKVM())
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeListAndWatchStream{ctx: ctx, sent: make(chan []*pluginapi.Device, 1)}

	done := make(chan error, 1)
	go func() { done <- p.ListAndWatch(&pluginapi.Empty{}, stream) }()

	// The first send happens immediately; wait for it rather than sleeping.
	select {
	case got := <-stream.sent:
		if len(got) != sharedDeviceCount {
			t.Errorf("sent %d devices, want %d", len(got), sharedDeviceCount)
		}
	case err := <-done:
		t.Fatalf("ListAndWatch returned before sending: %v", err)
	}

	select {
	case err := <-done:
		t.Fatalf("ListAndWatch returned while the stream was open: %v", err)
	default:
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("ListAndWatch() after cancel = %v, want nil", err)
	}
}

type fakeListAndWatchStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan []*pluginapi.Device
}

func (f *fakeListAndWatchStream) Context() context.Context { return f.ctx }

func (f *fakeListAndWatchStream) Send(resp *pluginapi.ListAndWatchResponse) error {
	f.sent <- resp.GetDevices()
	return nil
}
