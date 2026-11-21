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

// fcpoc starts one container inside a Firecracker-booted Kata guest, with the
// container rootfs on a virtio-blk device instead of the virtio-fs share.
// Proof of concept only; not part of the worker.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"

	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/kata"
	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/third_party/kata/agentpb"
)

// Guest path kata uses for a block-device container rootfs.
const guestRootfsFmt = "/run/kata-containers/shared/containers/%s/rootfs"

// containerd's default capability set.
var defaultCaps = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER", "CAP_MKNOD", "CAP_NET_RAW",
	"CAP_SETGID", "CAP_SETUID", "CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
}

func main() {
	vsock := flag.String("vsock", "", "Firecracker hybrid-vsock socket (vsock.uds_path)")
	dev := flag.String("dev", "/dev/vda", "guest block device holding the container rootfs (the read-only lower when -upper-dev is set)")
	upperDev := flag.String("upper-dev", "", "guest block device for a per-actor writable layer; empty runs directly on -dev")
	fstype := flag.String("fstype", "ext4", "rootfs filesystem type")
	cid := flag.String("cid", "ctr0", "container id")
	cmd := flag.String("cmd", "echo hello from firecracker; head -2 /etc/os-release; mount | grep ' / '; echo written > /probe && cat /probe", "shell command to run")
	detach := flag.Bool("detach", false, "return once the container has started instead of waiting for it")
	statsOnly := flag.Bool("stats", false, "only query an existing container's stats (e.g. after a restore)")
	flag.Parse()
	if *vsock == "" {
		log.Fatal("-vsock is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t0 := time.Now()
	step := func(name string) { log.Printf("%-16s %6.0f ms", name, float64(time.Since(t0).Microseconds())/1000) }

	ac, err := kata.DialAgent(ctx, *vsock)
	if err != nil {
		log.Fatal(err)
	}
	defer ac.Close()
	step("dial")

	if *statsOnly {
		st, err := ac.StatsContainer(ctx, *cid)
		if err != nil {
			log.Fatal(err)
		}
		step("stats")
		log.Printf("container %s: pids=%d memory=%d bytes", *cid, st.GetPidsStats().GetCurrent(), st.GetMemoryStats().GetUsage().GetUsage())
		return
	}

	if err := ac.CreateSandbox(ctx, &agentpb.CreateSandboxRequest{Hostname: "actor", SandboxId: "fcpoc"}); err != nil {
		log.Fatal(err)
	}
	step("create-sandbox")

	rootfs := fmt.Sprintf(guestRootfsFmt, *cid)
	pbSpec := kata.SpecToAgentPB(containerSpec(*cmd))
	pbSpec.Root = &agentpb.Root{Path: rootfs}
	pbSpec.Linux.CgroupsPath = "/actor/" + *cid
	if err := ac.CreateContainer(ctx, &agentpb.CreateContainerRequest{
		ContainerId: *cid,
		ExecId:      *cid,
		OCI:         pbSpec,
		Storages:    rootfsStorages(*cid, *dev, *upperDev, *fstype, rootfs),
	}); err != nil {
		log.Fatal(err)
	}
	step("create-container")

	if err := ac.CloseStdin(ctx, *cid, *cid); err != nil {
		log.Fatal(err)
	}
	if err := ac.StartContainer(ctx, *cid); err != nil {
		log.Fatal(err)
	}
	step("start-container")
	if *detach {
		return
	}

	var wg sync.WaitGroup
	for _, stderr := range []bool{false, true} {
		wg.Add(1)
		go func(stderr bool) {
			defer wg.Done()
			out := os.Stdout
			if stderr {
				out = os.Stderr
			}
			_, _ = io.Copy(out, kata.NewStdioReader(ctx, ac, *cid, *cid, stderr))
		}(stderr)
	}
	status, err := ac.WaitProcess(ctx, *cid, *cid)
	if err != nil {
		log.Fatal(err)
	}
	wg.Wait()
	step("exit")
	log.Printf("container exited with status %d", status)
	if status != 0 {
		os.Exit(1)
	}
}

func containerSpec(cmd string) *specs.Spec {
	return &specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			Args: []string{"/bin/sh", "-c", cmd},
			Env:  []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			Cwd:  "/",
			Capabilities: &specs.LinuxCapabilities{
				Bounding:  defaultCaps,
				Effective: defaultCaps,
				Permitted: defaultCaps,
			},
		},
		Hostname: "actor",
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
			{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		},
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{{Type: "pid"}, {Type: "ipc"}, {Type: "uts"}, {Type: "mount"}},
		},
	}
}

// rootfsStorages returns the agent storages that build the container rootfs, mounted
// in order. With an upper device the guest assembles overlay(lower = the shared
// read-only image, upper/work = the per-actor writable disk).
func rootfsStorages(cid, lowerDev, upperDev, fstype, rootfs string) []*agentpb.Storage {
	if upperDev == "" {
		return []*agentpb.Storage{{Driver: "mmioblk", Source: lowerDev, Fstype: fstype, MountPoint: rootfs}}
	}
	base := fmt.Sprintf("/run/kata-containers/shared/containers/%s", cid)
	lower, rw := base+"/lower", base+"/rw"
	upper, work := rw+"/upper", rw+"/work"
	const createDir = "io.katacontainers.volume.overlayfs.create_directory="
	return []*agentpb.Storage{
		{Driver: "mmioblk", Source: lowerDev, Fstype: fstype, Options: []string{"ro"}, MountPoint: lower},
		{Driver: "mmioblk", Source: upperDev, Fstype: "ext4", MountPoint: rw},
		{
			Driver:        "overlayfs",
			DriverOptions: []string{createDir + upper, createDir + work},
			Source:        "overlay",
			Fstype:        "overlay",
			Options:       []string{"lowerdir=" + lower, "upperdir=" + upper, "workdir=" + work},
			MountPoint:    rootfs,
		},
	}
}
