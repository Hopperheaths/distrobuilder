package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	incus "github.com/lxc/incus/v6/shared/util"
	"golang.org/x/sys/unix"

	"github.com/lxc/distrobuilder/shared"
)

type vm struct {
	imageFile  string
	loopDevice string
	rootFS     string
	rootfsDir  string
	bootfsDir  string
	size       uint64
	ctx        context.Context
}

func newVM(ctx context.Context, imageFile, rootfsDir, fs string, size uint64) (*vm, error) {
	if fs == "" {
		fs = "ext4"
	}

	if !slices.Contains([]string{"btrfs", "ext4"}, fs) {
		return nil, fmt.Errorf("Unsupported fs: %s", fs)
	}

	if size == 0 {
		size = 4294967296
	}

	return &vm{ctx: ctx, imageFile: imageFile, rootfsDir: rootfsDir, rootFS: fs, size: size}, nil
}

func (v *vm) getLoopDev() string {
	return v.loopDevice
}

func (v *vm) getRootfsDevFile() string {
	if v.loopDevice == "" {
		return ""
	}

	return fmt.Sprintf("%sp2", v.loopDevice)
}

func (v *vm) getUEFIDevFile() string {
	if v.loopDevice == "" {
		return ""
	}

	return fmt.Sprintf("%sp1", v.loopDevice)
}

func (v *vm) findRootfsDevUUID() (rootUUID string, err error) {
	rootfsDevFile := v.getRootfsDevFile()
	if rootfsDevFile == "" {
		err = fmt.Errorf("Failed to get rootfs device name.")
		return
	}

	var out strings.Builder
	if err = shared.RunCommand(v.ctx, nil, &out, "blkid", "-o", "export", rootfsDevFile); err != nil {
		err = fmt.Errorf("Failed to get rootfs device UUID: %w", err)
		return
	}

	fields := strings.Fields(out.String())
	for _, field := range fields {
		if strings.HasPrefix(field, "UUID=") {
			rootUUID = field
			break
		}
	}

	if rootUUID == "" {
		err = fmt.Errorf("No rootfs device UUID found")
		return
	}

	return
}

func (v *vm) createEmptyDiskImage() error {
	f, err := os.Create(v.imageFile)
	if err != nil {
		return fmt.Errorf("Failed to open %s: %w", v.imageFile, err)
	}

	defer f.Close()

	err = f.Chmod(0o600)
	if err != nil {
		return fmt.Errorf("Failed to chmod %s: %w", v.imageFile, err)
	}

	err = f.Truncate(int64(v.size))
	if err != nil {
		return fmt.Errorf("Failed to create sparse file %s: %w", v.imageFile, err)
	}

	return nil
}

func (v *vm) createPartitions(args ...[]string) error {
	if len(args) == 0 {
		args = [][]string{
			{"--zap-all"},
			{"--new=1::+100M", "-t 1:EF00"},
			{"--new=2::", "-t 2:8300"},
		}
	}

	for _, cmd := range args {
		err := shared.RunCommand(v.ctx, nil, nil, "sgdisk", append([]string{v.imageFile}, cmd...)...)
		if err != nil {
			return fmt.Errorf("Failed to create partitions: %w", err)
		}
	}

	return nil
}

func (v *vm) lsblkLoopDevice() (parseMajorMinor func(int) (uint32, uint32, error), num int, err error) {
	var out strings.Builder
	// Ensure the partitions are accessible. This part is usually only needed
	// if building inside of a container.
	err = shared.RunCommand(v.ctx, nil, &out, "lsblk", "--raw", "--output", "MAJ:MIN", "--noheadings", v.loopDevice)
	if err != nil {
		err = fmt.Errorf("Failed to list block devices: %w", err)
		return
	}

	lsblkOutput := strings.TrimSpace(out.String())
	// Output sample:
	// 7:1    -- loop device
	// 259:2  -- partition 1
	// 259:3  -- partition 2
	deviceNumbers := strings.Split(lsblkOutput, "\n")
	num = len(deviceNumbers)
	parseMajorMinor = func(i int) (major, minor uint32, err error) {
		if i >= num {
			err = fmt.Errorf("failed to parse major minor for %d >= %d", i, num)
			return
		}

		fields := strings.Split(deviceNumbers[i], ":")
		num, err := strconv.Atoi(fields[0])
		if err != nil {
			err = fmt.Errorf("Failed to parse %q: %w", fields[0], err)
			return
		}

		major = uint32(num)
		num, err = strconv.Atoi(fields[1])
		if err != nil {
			err = fmt.Errorf("Failed to parse %q: %w", fields[1], err)
			return
		}

		minor = uint32(num)
		return
	}

	return
}

func (v *vm) losetup() (err error) {
	var out strings.Builder
	err = shared.RunCommand(v.ctx, nil, &out, "losetup", "-P", "-f", "--show", v.imageFile)
	if err != nil {
		err = fmt.Errorf("Failed to setup loop device: %w", err)
		return
	}

	err = shared.RunCommand(v.ctx, nil, nil, "udevadm", "settle")
	if err != nil {
		err = fmt.Errorf("Failed to wait loop device ready: %w", err)
		return
	}

	v.loopDevice = strings.TrimSpace(out.String())
	return
}

func (v *vm) mountImage() (err error) {
	// If loopDevice is set, it probably is already mounted.
	if v.loopDevice != "" {
		return nil
	}

	err = v.losetup()
	if err != nil {
		return err
	}

	parseMajorMinor, num, err := v.lsblkLoopDevice()
	if err != nil {
		return
	} else if num != 3 {
		err = fmt.Errorf("Failed to list block devices")
		return
	}

	if !incus.PathExists(v.getUEFIDevFile()) {
		var major, minor uint32
		major, minor, err = parseMajorMinor(1)
		if err != nil {
			return
		}

		dev := unix.Mkdev(uint32(major), uint32(minor))

		err = unix.Mknod(v.getUEFIDevFile(), unix.S_IFBLK|0o644, int(dev))
		if err != nil {
			err = fmt.Errorf("Failed to create block device %q: %w", v.getUEFIDevFile(), err)
			return
		}
	}

	if !incus.PathExists(v.getRootfsDevFile()) {
		var major, minor uint32
		major, minor, err = parseMajorMinor(2)
		if err != nil {
			return
		}

		dev := unix.Mkdev(uint32(major), uint32(minor))

		err = unix.Mknod(v.getRootfsDevFile(), unix.S_IFBLK|0o644, int(dev))
		if err != nil {
			err = fmt.Errorf("Failed to create block device %q: %w", v.getRootfsDevFile(), err)
			return
		}
	}

	return
}

func (v *vm) umountImage() error {
	// If loopDevice is empty, the image probably isn't mounted.
	if v.loopDevice == "" || !incus.PathExists(v.loopDevice) {
		return nil
	}

	err := shared.RunCommand(v.ctx, nil, nil, "losetup", "-d", v.loopDevice)
	if err != nil {
		return fmt.Errorf("Failed to detach loop device: %w", err)
	}

	// Make sure that p1 and p2 are also removed.
	if incus.PathExists(v.getUEFIDevFile()) {
		err := os.Remove(v.getUEFIDevFile())
		if err != nil {
			return fmt.Errorf("Failed to remove file %q: %w", v.getUEFIDevFile(), err)
		}
	}

	if incus.PathExists(v.getRootfsDevFile()) {
		err := os.Remove(v.getRootfsDevFile())
		if err != nil {
			return fmt.Errorf("Failed to remove file %q: %w", v.getRootfsDevFile(), err)
		}
	}

	v.loopDevice = ""

	return nil
}

func (v *vm) createRootFS() error {
	if v.loopDevice == "" {
		return errors.New("Disk image not mounted")
	}

	switch v.rootFS {
	case "btrfs":
		err := shared.RunCommand(v.ctx, nil, nil, "mkfs.btrfs", "-f", "-L", "rootfs", v.getRootfsDevFile())
		if err != nil {
			return fmt.Errorf("Failed to create btrfs filesystem: %w", err)
		}

		// Create the root subvolume as well

		err = shared.RunCommand(v.ctx, nil, nil, "mount", "-t", v.rootFS, v.getRootfsDevFile(), v.rootfsDir)
		if err != nil {
			return fmt.Errorf("Failed to mount %q at %q: %w", v.getRootfsDevFile(), v.rootfsDir, err)
		}

		defer func() {
			_ = v.umountPartition(v.rootfsDir)
		}()

		return shared.RunCommand(v.ctx, nil, nil, "btrfs", "subvolume", "create", fmt.Sprintf("%s/@", v.rootfsDir))
	case "ext4":
		return shared.RunCommand(v.ctx, nil, nil, "mkfs.ext4", "-F", "-b", "4096", "-i 8192", "-m", "0", "-L", "rootfs", "-E", "resize=536870912", v.getRootfsDevFile())
	}

	return nil
}

func (v *vm) createUEFIFS() error {
	if v.loopDevice == "" {
		return errors.New("Disk image not mounted")
	}

	return shared.RunCommand(v.ctx, nil, nil, "mkfs.vfat", "-F", "32", "-n", "UEFI", v.getUEFIDevFile())
}

func (v *vm) mountRootPartition() error {
	if v.loopDevice == "" {
		return errors.New("Disk image not mounted")
	}

	switch v.rootFS {
	case "btrfs":
		return shared.RunCommand(v.ctx, nil, nil, "mount", v.getRootfsDevFile(), v.rootfsDir, "-t", v.rootFS, "-o", "defaults,discard,nobarrier,commit=300,noatime,subvol=/@")
	case "ext4":
		return shared.RunCommand(v.ctx, nil, nil, "mount", v.getRootfsDevFile(), v.rootfsDir, "-t", v.rootFS, "-o", "discard,nobarrier,commit=300,noatime,data=writeback")
	}

	return nil
}

func (v *vm) mountUEFIPartition() error {
	if v.loopDevice == "" {
		return errors.New("Disk image not mounted")
	}

	v.bootfsDir = filepath.Join(v.rootfsDir, "boot", "efi")

	err := os.MkdirAll(v.bootfsDir, 0o755)
	if err != nil {
		return fmt.Errorf("Failed to create directory %q: %w", v.bootfsDir, err)
	}

	return shared.RunCommand(v.ctx, nil, nil, "mount", "-t", "vfat", v.getUEFIDevFile(), v.bootfsDir, "-o", "discard")
}

func (v *vm) umountPartition(mountpoint string) (err error) {
	err = v.checkMountpoint(mountpoint)
	if err != nil {
		return
	}

	return shared.RunCommand(v.ctx, nil, nil, "umount", "-R", mountpoint)
}

func (v *vm) checkMountpoint(mountpoint string) (err error) {
	err = shared.RunCommand(v.ctx, nil, nil, "mountpoint", mountpoint)
	if err != nil {
		err = fmt.Errorf("%s not mounted: %w", mountpoint, err)
	}

	return err
}
