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

package main

// Firecracker backend for the micro-VM worker (proof of concept).
//
// Selected when ATE_MICROVM_FIRECRACKER names a firecracker binary. It reuses the
// Kata kernel, guest image and kata-agent, and differs from the Cloud Hypervisor
// path in three ways:
//
//   - No virtio-fs. Each container's image is an ext4 block image (read-only), the
//     actor gets one writable disk for every container's overlay upper, and the
//     durable-dir volumes become one more disk. kata-agent mounts the disks and
//     builds the overlays in the guest.
//   - Firecracker runs chrooted into a per-actor directory with fixed paths inside,
//     so a snapshot restores into any actor without drive_overrides.
//   - Firecracker opens the tap by name, so it runs in the actor's network namespace.
//
// Only MEMORY snapshots are supported. A snapshot ships the VM state, guest memory
// and every disk image, plus fcManifestFile.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/child"
	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/fc"
	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/kata"
	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/third_party/kata/agentpb"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/ateworker"
	"go.ate.dev/substrate/internal/ateworker/actornet"
	"go.ate.dev/substrate/internal/ateworker/cgroup"
	"go.ate.dev/substrate/internal/ateworker/phaselog"
	"go.ate.dev/substrate/internal/imagecache"
	"go.ate.dev/substrate/internal/netns"
	"go.ate.dev/substrate/internal/ocispec"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/wakeupprobe"
	"go.ate.dev/substrate/internal/workerstats"
)

const (
	fcEnvBinary    = "ATE_MICROVM_FIRECRACKER"
	fcManifestFile = "fc.json"
	fcStateFile    = "vmstate"
	fcMemFile      = "mem"
	fcUpperDisk    = "upper.ext4"
	fcDurableDisk  = "durable.ext4"
	fcTapName      = "tap0_kata"
	fcUpperBytes   = 8 << 30
	// Guest mount points for the disks the overlays are built from.
	fcGuestBase = "/run/kata-containers/fc"
	fcGuestRW   = fcGuestBase + "/rw"
)

// fcManifest records what a Firecracker snapshot holds, in drive order.
type fcManifest struct {
	Containers []string `json:"containers"` // guest container ids
	Durable    bool     `json:"durable"`
	VCPUs      int      `json:"vcpus"`
	MemMiB     int      `json:"mem_mib"`
}

func fcLowerDisk(i int) string { return fmt.Sprintf("lower-%d.ext4", i) }

// fcDisks lists the chroot disk files in drive order: lowers, upper, durable.
func (m fcManifest) disks() []string {
	var d []string
	for i := range m.Containers {
		d = append(d, fcLowerDisk(i))
	}
	d = append(d, fcUpperDisk)
	if m.Durable {
		d = append(d, fcDurableDisk)
	}
	return d
}

func (m fcManifest) drives() []fc.Drive {
	var out []fc.Drive
	for i, name := range m.disks() {
		out = append(out, fc.Drive{DriveID: fmt.Sprintf("d%d", i), PathOnHost: "/" + name, IsReadOnly: i < len(m.Containers)})
	}
	return out
}

func guestDev(i int) string { return fmt.Sprintf("/dev/vd%c", 'a'+i) }

// guestStorages are the kata-agent sandbox storages, mounted in order: each image,
// the writable disk, each container's overlay at its usual shared rootfs path, and
// the durable disk where the durable-dir volumes are normally shared.
func (m fcManifest) guestStorages() []*agentpb.Storage {
	const createDir = "io.katacontainers.volume.overlayfs.create_directory="
	n := len(m.Containers)
	var st []*agentpb.Storage
	for i, cid := range m.Containers {
		st = append(st, &agentpb.Storage{Driver: "mmioblk", Source: guestDev(i), Fstype: "ext4", Options: []string{"ro"}, MountPoint: path.Join(fcGuestBase, cid, "lower")})
	}
	st = append(st, &agentpb.Storage{Driver: "mmioblk", Source: guestDev(n), Fstype: "ext4", MountPoint: fcGuestRW})
	for _, cid := range m.Containers {
		upper, work := path.Join(fcGuestRW, "upper", cid), path.Join(fcGuestRW, "work", cid)
		st = append(st, &agentpb.Storage{
			Driver:        "overlayfs",
			DriverOptions: []string{createDir + upper, createDir + work},
			Source:        "overlay",
			Fstype:        "overlay",
			Options:       []string{"lowerdir=" + path.Join(fcGuestBase, cid, "lower"), "upperdir=" + upper, "workdir=" + work},
			MountPoint:    kata.GuestSharedRootfs(cid),
		})
	}
	if m.Durable {
		st = append(st, &agentpb.Storage{Driver: "mmioblk", Source: guestDev(n + 1), Fstype: "ext4", MountPoint: path.Join(ocispec.GuestSharedDir, ocispec.ShareDurable)})
	}
	return st
}

func fcRoot(actorUID string) string { return filepath.Join(kata.VMDir(actorUID), "fc") }

// fcPrepareRoot lays out the chroot: the firecracker binary, kernel and guest image
// bound read-only, and /dev/kvm and /dev/net/tun bound in. CleanupSandboxState
// unmounts and removes it with the rest of VMDir.
func (s *AteWorkerService) fcPrepareRoot(root, kernel, image string) error {
	if err := os.MkdirAll(filepath.Join(root, "dev", "net"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, fc.SnapDir[1:]), 0o700); err != nil {
		return err
	}
	for _, b := range []struct {
		src, dst string
		ro       bool
	}{
		{s.fcBinary, fc.Binary, true},
		{kernel, fc.Kernel, true},
		{image, fc.GuestImage, true},
		{"/dev/kvm", "/dev/kvm", false},
		{"/dev/net/tun", "/dev/net/tun", false},
	} {
		if err := bindFile(b.src, filepath.Join(root, b.dst[1:]), b.ro); err != nil {
			return err
		}
	}
	return nil
}

func bindFile(src, dst string, ro bool) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return err
	}
	_ = f.Close()
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %q -> %q: %w", src, dst, err)
	}
	if ro {
		if err := unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount %q read-only: %w", dst, err)
		}
	}
	return nil
}

func bindDir(src, dst string) error {
	_ = unix.Unmount(dst, unix.MNT_DETACH)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %q -> %q: %w", src, dst, err)
	}
	return nil
}

// mkfsExt4 creates a sparse ext4 image of size bytes, populated from srcDir if set.
func mkfsExt4(ctx context.Context, image string, size int64, srcDir string, journal bool) error {
	f, err := os.Create(image)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return err
	}
	_ = f.Close()
	args := []string{"-q", "-F", "-t", "ext4"}
	if !journal {
		args = append(args, "-O", "^has_journal")
	}
	if srcDir != "" {
		args = append(args, "-d", srcDir)
	}
	args = append(args, image)
	if out, err := exec.CommandContext(ctx, "mkfs.ext4", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", image, err, out)
	}
	return nil
}

// treeBytes sums the sizes of the regular files under dir.
func treeBytes(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n, err
}

// imageSizeFor leaves generous room: the image is sparse, so unused space is free.
func imageSizeFor(contentBytes int64) int64 {
	const mib = 1 << 20
	size := 2*contentBytes + 256*mib
	return (size + mib - 1) / mib * mib
}

// copySparse copies src to dst, skipping holes.
func copySparse(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	size := info.Size()
	if err := out.Truncate(size); err != nil {
		return err
	}
	fd := int(in.Fd())
	for off := int64(0); off < size; {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break // no data past off
			}
			return err
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			return err
		}
		if _, err := in.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := out.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(out, in, hole-data); err != nil {
			return err
		}
		off = hole
	}
	return out.Close()
}

// fcLaunch starts Firecracker in the actor's network namespace and cgroup leaf.
func (s *AteWorkerService) fcLaunch(ctx context.Context, actorUID, root string, cfg *fc.Config, leaf *cgroup.ActorLeaf) (*child.Process, *fc.Client, error) {
	nsHandle := s.sandboxNetNS(actorUID)
	return fc.Launch(ctx, fc.LaunchOptions{
		Root:        root,
		Config:      cfg,
		SysProcAttr: leaf.SysProcAttr(),
		Stdout:      slogWriter{ctx},
		Stderr:      slogWriter{ctx},
		Start: func(cmd *exec.Cmd) (*child.Process, error) {
			var p *child.Process
			err := netns.Do(ctx, nsHandle, func(context.Context) error {
				var err error
				p, err = child.Start(cmd)
				return err
			})
			return p, err
		},
	})
}

// fcBootArgs is the guest cmdline: the same root and init as the Cloud Hypervisor
// path, with the serial console only in debug mode (Firecracker has no virtio-console).
func (s *AteWorkerService) fcBootArgs() string {
	args := "root=/dev/pmem0p1 rootflags=dax,data=ordered,errors=remount-ro ro rootfstype=ext4 " +
		"panic=1 reboot=k no_timer_check noreplace-smp pci=off swiotlb=noforce " +
		initParams(true) + " " + s.kernelParams()
	if runtime.GOARCH == "amd64" {
		args += " clocksource=kvm-clock"
	}
	if s.guestDebug {
		return args + " console=ttyS0"
	}
	return args + " 8250.nr_uarts=0 quiet"
}

// fcTap builds the actor's persistent tap; Firecracker opens it by name.
func (s *AteWorkerService) fcTap(ctx context.Context, actorUID string) error {
	files, err := setupActorTap(ctx, s.sandboxNetNS(actorUID), fcTapName, 1)
	for _, f := range files {
		_ = f.Close()
	}
	return err
}

func fcUnsupported(containers []*ateworkerpb.Container) error {
	if hasCsiVolumes(containers) || hasSystemInfoVolumes(containers) {
		return apierror.FailedPrecondition("the Firecracker worker does not support CSI or system-info volumes yet")
	}
	for _, c := range containers {
		if len(c.GetImageVolumeMounts()) > 0 {
			return apierror.FailedPrecondition("the Firecracker worker does not support image volumes yet")
		}
	}
	return nil
}

// fcStartGuest dials the agent, creates the sandbox with the disk storages,
// configures networking and starts the containers. Cold boot only.
func (s *AteWorkerService) fcStartGuest(ctx context.Context, actorUID, vsock string, m fcManifest, ctrs []actorContainer) (*kata.AgentClient, error) {
	if !waitForFile(vsock, 15*time.Second) {
		return nil, fmt.Errorf("kata-agent vsock socket %q did not appear", vsock)
	}
	ac, err := dialAgentRetry(ctx, vsock, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("while dialing kata-agent: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = ac.Close()
		}
	}()
	sbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = ac.CreateSandbox(sbCtx, &agentpb.CreateSandboxRequest{
		Hostname:  ctrs[0].spec.Hostname,
		SandboxId: actorUID,
		Storages:  m.guestStorages(),
	})
	cancel()
	if err != nil {
		if s.guestDebug {
			slog.ErrorContext(ctx, "fc sandbox failed; guest dump", slog.String("dump",
				kata.DebugConsoleDump(ctx, vsock, "ls -l /dev/vd* /dev/pmem*; grep -E 'vd|overlay|kata' /proc/mounts")))
		}
		return nil, fmt.Errorf("while creating agent sandbox: %w", err)
	}
	netCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err = s.configureGuestNetwork(netCtx, ac, actorTapMTU)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("while configuring guest network: %w", err)
	}
	for _, c := range ctrs {
		if err := s.startRootfsContainer(ctx, ac, vsock, c); err != nil {
			return nil, err
		}
	}
	ok = true
	return ac, nil
}

// coldBootFirecracker is coldBootActor for the Firecracker backend.
func (s *AteWorkerService) coldBootFirecracker(ctx context.Context, p actorBootParams) (retErr error) {
	actorUID := p.actorUID
	if p.rootfsUpperSnapshotDir != "" {
		return apierror.FailedPrecondition("the Firecracker worker supports only MEMORY snapshots")
	}
	if err := fcUnsupported(p.containers); err != nil {
		return err
	}
	kernel, image := p.assetPaths[assetKernel], p.assetPaths[assetImage]
	if kernel == "" || image == "" {
		return fmt.Errorf("ateworker-microvm requires %q and %q asset paths", assetKernel, assetImage)
	}
	tStart := time.Now()
	egress, err := s.tunnel.PrepareEgress(ctx, p.attribution(), p.egressGateway)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = s.tunnel.Deactivate(cleanupCtx, p.attribution())
			s.cleanupSandboxState(cleanupCtx, actorUID)
			if err := imagecache.UnmountAllUnder(p.actorDirs.GetOciBundleDir()); err != nil {
				slog.WarnContext(ctx, "Failed to unmount bundle rootfs overlays after Run failure", slog.Any("err", err))
			}
		}
	}()
	s.cleanupSandboxState(ctx, actorUID)
	root := fcRoot(actorUID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := s.tunnel.ActivateEgress(p.attribution(), egress); err != nil {
		return err
	}
	vcpus := p.size.VCPUs()
	if vcpus == 0 {
		return errors.New("actor has no cpu limit to size the micro-VM's vCPUs")
	}
	memMiB, err := resolveGuestMemMiB(p.size.MemoryBytes)
	if err != nil {
		return err
	}
	ctrs, err := s.buildActorContainers(actorUID, p.actorDirs, p.containers)
	if err != nil {
		return err
	}
	if err := checkResourceEnvelope(ctrs, guestEnvelope{memMiB: memMiB, vcpus: vcpus}); err != nil {
		return err
	}
	if err := s.fcPrepareRoot(root, kernel, image); err != nil {
		return err
	}
	tPrep := time.Now()

	m := fcManifest{Durable: hasDurableVolumes(p.containers), VCPUs: vcpus, MemMiB: memMiB}
	for i, c := range ctrs {
		m.Containers = append(m.Containers, c.id)
		n, err := treeBytes(c.bundleRootfs)
		if err != nil {
			return fmt.Errorf("while sizing the image of %q: %w", c.name, err)
		}
		if err := mkfsExt4(ctx, filepath.Join(root, fcLowerDisk(i)), imageSizeFor(n), c.bundleRootfs, false); err != nil {
			return err
		}
	}
	if err := mkfsExt4(ctx, filepath.Join(root, fcUpperDisk), fcUpperBytes, "", true); err != nil {
		return err
	}
	if m.Durable {
		dir := p.actorDirs.GetDurableDirVolumeMountsDir()
		n, err := treeBytes(dir)
		if err != nil {
			return err
		}
		if err := mkfsExt4(ctx, filepath.Join(root, fcDurableDisk), imageSizeFor(n)+(1<<30), dir, true); err != nil {
			return err
		}
	}
	tDisks := time.Now()

	if err := s.fcTap(ctx, actorUID); err != nil {
		return fmt.Errorf("while building tap: %w", err)
	}
	leaf, err := cgroup.OpenActorLeaf(actorUID, p.size.MilliCPU, s.leafMemoryHigh(p.size.MemoryBytes))
	if err != nil {
		return err
	}
	defer leaf.Close()
	cfg := &fc.Config{
		BootSource:    fc.BootSource{KernelImagePath: fc.Kernel, BootArgs: s.fcBootArgs()},
		MachineConfig: fc.MachineConfig{VcpuCount: vcpus, MemSizeMiB: memMiB},
		Drives:        m.drives(),
		Pmem:          []fc.Pmem{{ID: guestImagePmemID, PathOnHost: fc.GuestImage, ReadOnly: true}},
		Vsock:         &fc.Vsock{GuestCID: 3, UDSPath: fc.VsockSocket},
		Network:       []fc.NetInterface{{IfaceID: actornet.ActorVethName, HostDevName: fcTapName, GuestMAC: actorGuestMAC}},
		Entropy:       &struct{}{},
	}
	proc, _, err := s.fcLaunch(ctx, actorUID, root, cfg, leaf)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			proc.Stop()
		}
	}()
	tBooted := time.Now()
	if err := os.WriteFile(filepath.Join(root, fcManifestFile), mustJSON(m), 0o600); err != nil {
		return err
	}

	vsock := filepath.Join(root, fc.VsockSocket[1:])
	ac, err := s.fcStartGuest(ctx, actorUID, vsock, m, ctrs)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = ac.Close()
		}
	}()
	tContainers := time.Now()
	if err := wakeupprobe.WaitAll(ctx, p.containers, actornet.ActorVethIP, wakeupprobe.DialFunc(s.sandboxDialer(actorUID))); err != nil {
		return fmt.Errorf("while waiting for container wakeup probe: %w", err)
	}
	slog.InfoContext(ctx, "Firecracker boot phases", slog.String("id", actorUID),
		slog.Duration("prep", tPrep.Sub(tStart)),
		slog.Duration("disks", tDisks.Sub(tPrep)),
		slog.Duration("launch", tBooted.Sub(tDisks)),
		slog.Duration("agent_and_containers", tContainers.Sub(tBooted)),
		slog.Duration("wakeup_probe", time.Since(tContainers)),
		slog.Duration("total", time.Since(tStart)))

	ra := &runningActor{chProc: proc, apiSocket: filepath.Join(root, fc.APISocket[1:]), fcRoot: root,
		memoryBytes: p.size.MemoryBytes, baseID: actorUID, guestAgent: ac, workloadIDs: workloadIDs(ctrs)}
	return s.fcActivate(ctx, p, ra, ac, ctrs)
}

// fcActivate is the common tail of cold boot and restore: ingress, bookkeeping,
// log forwarding and stats.
func (s *AteWorkerService) fcActivate(ctx context.Context, p actorBootParams, ra *runningActor, ac *kata.AgentClient, ctrs []actorContainer) error {
	if err := s.tunnel.ActivateIngress(p.attribution(), s.sandboxDialer(p.actorUID)); err != nil {
		return err
	}
	ra.memory.Resume(uint64(s.leafMemoryHigh(p.size.MemoryBytes)))
	s.setRunningVM(p.actorUID, ra)
	go s.watchVM(p.attribution(), p.actorDirs, ra)
	attribution := p.actorAttribution()
	for _, c := range ctrs {
		s.startActorLogForwarding(ac, attribution, c.id, c.name)
	}
	s.setGuestStats(p.actorUID, &guestStatsTarget{actorUID: p.actorUID, agent: ac, workloadIDs: ra.workloadIDs})
	if hosted := s.lookupActor(p.actorUID); hosted != nil {
		go s.recordInitial(context.WithoutCancel(ctx), hosted)
	}
	return nil
}

// restoreFirecracker restores a MEMORY snapshot taken by checkpointFirecracker.
func (s *AteWorkerService) restoreFirecracker(ctx context.Context, p actorBootParams, scope ateworkerpb.SnapshotFidelity, restoreDir string, tStart time.Time) (retErr error) {
	actorUID := p.actorUID
	b, err := os.ReadFile(filepath.Join(restoreDir, fcManifestFile))
	if err != nil {
		return apierror.FailedPrecondition("snapshot was not taken by the Firecracker worker: %v", err)
	}
	var m fcManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("while reading %s: %w", fcManifestFile, err)
	}
	kernel, image := p.assetPaths[assetKernel], p.assetPaths[assetImage]
	egress, err := s.tunnel.PrepareEgress(ctx, p.attribution(), p.egressGateway)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = s.tunnel.Deactivate(cleanupCtx, p.attribution())
			s.cleanupSandboxState(cleanupCtx, actorUID)
		}
	}()
	s.cleanupSandboxState(ctx, actorUID)
	root := fcRoot(actorUID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := s.fcPrepareRoot(root, kernel, image); err != nil {
		return err
	}
	if err := bindDir(restoreDir, filepath.Join(root, fc.SnapDir[1:])); err != nil {
		return err
	}
	tPrep := time.Now()
	// Images are read-only: bind them. The writable disks are copied, so the
	// snapshot stays reusable.
	for i, name := range m.disks() {
		src, dst := filepath.Join(restoreDir, name), filepath.Join(root, name)
		if i < len(m.Containers) {
			err = bindFile(src, dst, true)
		} else {
			err = copySparse(src, dst)
		}
		if err != nil {
			return fmt.Errorf("while staging %s: %w", name, err)
		}
	}
	tDisks := time.Now()
	if err := s.fcTap(ctx, actorUID); err != nil {
		return fmt.Errorf("while building tap: %w", err)
	}
	tTap := time.Now()
	leaf, err := cgroup.OpenActorLeaf(actorUID, p.size.MilliCPU, s.leafMemoryHigh(p.size.MemoryBytes))
	if err != nil {
		return err
	}
	defer leaf.Close()
	proc, client, err := s.fcLaunch(ctx, actorUID, root, nil, leaf)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			proc.Stop()
		}
	}()
	tLaunch := time.Now()
	if err := client.SnapshotLoad(ctx, path.Join(fc.SnapDir, fcStateFile), path.Join(fc.SnapDir, fcMemFile)); err != nil {
		return fmt.Errorf("while loading snapshot: %w", err)
	}
	tVMRestore := time.Now()
	if err := os.WriteFile(filepath.Join(root, fcManifestFile), b, 0o600); err != nil {
		return err
	}
	ac, err := dialAgentRetry(ctx, filepath.Join(root, fc.VsockSocket[1:]), 15*time.Second)
	if err != nil {
		return restoreGuestReadyError(ctx, "dialing kata-agent after restore", err)
	}
	defer func() {
		if retErr != nil {
			_ = ac.Close()
		}
	}()
	if err := reseedGuestCRNG(ctx, ac); err != nil {
		return restoreGuestReadyError(ctx, "reseeding guest CRNG", err)
	}
	if err := s.tunnel.ActivateEgress(p.attribution(), egress); err != nil {
		return err
	}
	if err := wakeupprobe.WaitAll(ctx, p.containers, actornet.ActorVethIP, wakeupprobe.DialFunc(s.sandboxDialer(actorUID))); err != nil {
		return fmt.Errorf("while waiting for container wakeup probe: %w", err)
	}
	dWakeup := time.Since(tVMRestore)
	phaselog.LogSnapshotPhases(ctx, "Restore timing breakdown", p.actorAttribution(), scope,
		phaselog.RestoreDurationKey, nil, []phaselog.Phase{
			{Name: phasePrep, D: tPrep.Sub(tStart)},
			{Name: phaseLowers, D: tDisks.Sub(tPrep)},
			{Name: phaseTap, D: tTap.Sub(tDisks)},
			{Name: phaseVMMLaunch, D: tLaunch.Sub(tTap)},
			{Name: phaseVMRestore, D: tVMRestore.Sub(tLaunch)},
			{Name: phaseWakeupProbe, D: dWakeup},
			{Name: phaseTotal, D: time.Since(tStart)},
		})

	ctrs := make([]actorContainer, len(p.containers))
	for i, c := range p.containers {
		ctrs[i] = actorContainer{name: c.GetName(), id: ocispec.GuestContainerID(c.GetName())}
	}
	ra := &runningActor{chProc: proc, apiSocket: filepath.Join(root, fc.APISocket[1:]), fcRoot: root,
		memoryBytes: p.size.MemoryBytes, baseID: actorUID, guestAgent: ac, workloadIDs: workloadIDs(ctrs),
		snapshotIsSelfContained: true}
	return s.fcActivate(ctx, p, ra, ac, ctrs)
}

// fcCheckpointPhases are what checkpointFirecracker measured.
type fcCheckpointPhases struct {
	pause, snapshot, disks, teardown time.Duration
}

// checkpointFirecracker takes a MEMORY snapshot of a Firecracker actor into the
// checkpoint dir: VM state, guest memory, every disk image and the manifest.
func (s *AteWorkerService) checkpointFirecracker(ctx context.Context, req *ateworkerpb.CheckpointWorkloadRequest, ra *runningActor, hosted *hostedActor, ph *fcCheckpointPhases) (*ateworkerpb.CheckpointWorkloadResponse, error) {
	scope := req.GetFidelity()
	if scope != ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		return nil, apierror.FailedPrecondition("the Firecracker worker supports only MEMORY snapshots, not %v", scope)
	}
	actorUID := req.GetActorUid()
	actorDirs := req.GetActorDirs()
	attribution := workerstats.ActorAttributionFromRequest(req)
	b, err := os.ReadFile(filepath.Join(ra.fcRoot, fcManifestFile))
	if err != nil {
		return nil, err
	}
	var m fcManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	client := fc.NewClient(ra.apiSocket)
	if hosted != nil {
		s.readFinal(ctx, hosted)
	}
	t := time.Now()
	if err := client.Pause(ctx); err != nil {
		return nil, fmt.Errorf("while pausing guest: %w", err)
	}
	ph.pause = time.Since(t)

	checkpointDir := actorDirs.GetCheckpointDir()
	if err := os.RemoveAll(checkpointDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(checkpointDir, 0o700); err != nil {
		return nil, err
	}
	snap := filepath.Join(ra.fcRoot, fc.SnapDir[1:])
	if err := bindDir(checkpointDir, snap); err != nil {
		return nil, err
	}
	t = time.Now()
	err = client.SnapshotCreate(ctx, path.Join(fc.SnapDir, fcStateFile), path.Join(fc.SnapDir, fcMemFile))
	ph.snapshot = time.Since(t)
	_ = unix.Unmount(snap, unix.MNT_DETACH)
	if err != nil {
		return nil, fmt.Errorf("while creating snapshot: %w", err)
	}
	// Firecracker drained and fsynced the disks while creating the snapshot.
	t = time.Now()
	for _, name := range m.disks() {
		if err := copySparse(filepath.Join(ra.fcRoot, name), filepath.Join(checkpointDir, name)); err != nil {
			return nil, fmt.Errorf("while capturing %s: %w", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, fcManifestFile), b, 0o600); err != nil {
		return nil, err
	}
	ph.disks = time.Since(t)
	files, err := listFiles(checkpointDir)
	if err != nil {
		return nil, err
	}
	t = time.Now()
	if err := s.terminateWorkload(ctx, attribution, actorDirs); err != nil {
		slog.WarnContext(ctx, "failed to terminate workload after checkpoint", slog.String("actorUID", actorUID), slog.Any("err", err))
	}
	ph.teardown = time.Since(t)
	s.actorLogger.EmitLifecycleLog(ctx, "Actor checkpointed", attribution)
	s.recordFinalIfEnded(ctx, hosted)
	resp := &ateworkerpb.CheckpointWorkloadResponse{SnapshotFiles: files}
	if err := ateworker.RecordCheckpoint(checkpointDir, s.bootID, scope, resp); err != nil {
		slog.WarnContext(ctx, "Failed to record checkpoint completion marker", slog.String("actorUID", actorUID), slog.Any("err", err))
	}
	return resp, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
