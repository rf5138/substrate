//go:build linux

// Copyright 2026 The Agent Substrate Authors
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

// Package fc drives a Firecracker VMM over its API socket.
//
// Each actor's files live in its own directory and Firecracker is given their
// real host paths. A snapshot records those paths, so restoring it into another
// actor passes that actor's paths as overrides (drive_overrides and
// pmem_overrides need Firecracker built with #6402, or the 1.18 backport).
package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/child"
)

// File names inside an actor's directory.
const (
	APISocket   = "api.sock"
	VsockSocket = "v.sock"
)

// Config is Firecracker's --config-file document.
type Config struct {
	BootSource    BootSource     `json:"boot-source"`
	MachineConfig MachineConfig  `json:"machine-config"`
	Drives        []Drive        `json:"drives"`
	Pmem          []Pmem         `json:"pmem,omitempty"`
	Vsock         *Vsock         `json:"vsock,omitempty"`
	Network       []NetInterface `json:"network-interfaces,omitempty"`
	Entropy       *struct{}      `json:"entropy,omitempty"`
}

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type MachineConfig struct {
	VcpuCount       int  `json:"vcpu_count"`
	MemSizeMiB      int  `json:"mem_size_mib"`
	TrackDirtyPages bool `json:"track_dirty_pages,omitempty"`
}

type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type Pmem struct {
	ID         string `json:"id"`
	PathOnHost string `json:"path_on_host"`
	RootDevice bool   `json:"root_device"`
	ReadOnly   bool   `json:"read_only"`
}

type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type NetInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac"`
}

// LaunchOptions starts a Firecracker process for one actor.
type LaunchOptions struct {
	// Binary is the firecracker executable.
	Binary string
	// Dir is the actor's directory; the API socket is created in it.
	Dir string
	// Config, if set, is written into Dir and passed as --config-file, which
	// configures and boots the VM. Nil starts a bare VMM (for restore).
	Config *Config
	// Start runs cmd.Start; the caller uses it to start the process in the
	// actor's network namespace, where the tap lives.
	Start       func(*exec.Cmd) (*child.Process, error)
	SysProcAttr *syscall.SysProcAttr
	Stdout      io.Writer
	Stderr      io.Writer
}

// Launch starts Firecracker and waits for its API socket.
func Launch(ctx context.Context, o LaunchOptions) (*child.Process, *Client, error) {
	sock := filepath.Join(o.Dir, APISocket)
	args := []string{"--api-sock", sock}
	if o.Config != nil {
		b, err := json.Marshal(o.Config)
		if err != nil {
			return nil, nil, err
		}
		cfg := filepath.Join(o.Dir, "config.json")
		if err := os.WriteFile(cfg, b, 0o600); err != nil {
			return nil, nil, err
		}
		args = append(args, "--config-file", cfg)
	}
	_ = os.Remove(sock)
	// Not CommandContext: the VMM outlives the RPC that launched it.
	cmd := exec.Command(o.Binary, args...)
	cmd.Dir = o.Dir
	cmd.Stdout = o.Stdout
	cmd.Stderr = o.Stderr
	cmd.SysProcAttr = o.SysProcAttr
	start := o.Start
	if start == nil {
		start = child.Start
	}
	proc, err := start(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("while starting firecracker: %w", err)
	}
	c := NewClient(sock)
	if err := c.WaitReady(ctx, 15*time.Second); err != nil {
		proc.Stop()
		return nil, nil, err
	}
	return proc, c, nil
}

// Client talks to one Firecracker API socket.
type Client struct {
	http *http.Client
}

func NewClient(socket string) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("firecracker %s %s: HTTP %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// WaitReady polls GET / until the API answers.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	end := time.Now().Add(timeout)
	for {
		err := c.do(ctx, http.MethodGet, "/", nil)
		if err == nil {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("firecracker API not ready: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (c *Client) setState(ctx context.Context, state string) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": state})
}

func (c *Client) Pause(ctx context.Context) error  { return c.setState(ctx, "Paused") }
func (c *Client) Resume(ctx context.Context) error { return c.setState(ctx, "Resumed") }

// SnapshotCreate writes a diff snapshot of the paused VM to the given paths:
// a sparse memory file holding only the pages written since boot or the last
// restore (dirty-page tracking must be on).
func (c *Client) SnapshotCreate(ctx context.Context, statePath, memPath string) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", map[string]any{
		"snapshot_type":       "Diff",
		"snapshot_path":       statePath,
		"mem_file_path":       memPath,
		"sync_snapshot_files": false,
	})
}

// Overrides point a snapshot's devices at this actor's files on restore.
type Overrides struct {
	Drives []DriveOverride `json:"drive_overrides,omitempty"`
	Pmem   []PmemOverride  `json:"pmem_overrides,omitempty"`
	Vsock  *VsockOverride  `json:"vsock_override,omitempty"`
}

type DriveOverride struct {
	DriveID    string `json:"drive_id"`
	PathOnHost string `json:"path_on_host"`
}

type PmemOverride struct {
	ID         string `json:"id"`
	PathOnHost string `json:"path_on_host"`
}

type VsockOverride struct {
	UDSPath string `json:"uds_path"`
}

// SnapshotLoad restores a snapshot into a bare VMM and resumes it. Guest memory is
// a private mapping of memPath, so it loads lazily as the guest touches it.
func (c *Client) SnapshotLoad(ctx context.Context, statePath, memPath string, ov Overrides) error {
	body := map[string]any{
		"snapshot_path":     statePath,
		"mem_backend":       map[string]string{"backend_type": "File", "backend_path": memPath},
		"clock_realtime":    true,
		"track_dirty_pages": true,
		"resume_vm":         true,
	}
	if len(ov.Drives) > 0 {
		body["drive_overrides"] = ov.Drives
	}
	if len(ov.Pmem) > 0 {
		body["pmem_overrides"] = ov.Pmem
	}
	if ov.Vsock != nil {
		body["vsock_override"] = ov.Vsock
	}
	return c.do(ctx, http.MethodPut, "/snapshot/load", body)
}
