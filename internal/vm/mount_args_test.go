//go:build linux

package vm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fwCfgMounts returns the value of the opt/uni/mounts fw_cfg arg, or "".
func fwCfgMounts(args []string) string {
	for i, a := range args {
		if a == "-fw_cfg" && i+1 < len(args) && strings.HasPrefix(args[i+1], "name=opt/uni/mounts,string=") {
			return strings.TrimPrefix(args[i+1], "name=opt/uni/mounts,string=")
		}
	}
	return ""
}

func TestQEMUMountArgs_InjectsLabeledVolume(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
		Volumes: []VolumeMount{
			{DiskPath: "/vol/mysql.img", GuestPath: "/var/lib/mysql", Label: "mysqldata"},
		},
	})
	require.Equal(t, "mysqldata:/var/lib/mysql", fwCfgMounts(args))
	// The volume is also attached as a virtio-blk drive at index 1.
	require.Contains(t, strings.Join(args, " "), "file=/vol/mysql.img,format=raw,if=virtio,index=1")
}

func TestQEMUMountArgs_MultipleVolumesNewlineJoined(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
		Volumes: []VolumeMount{
			{DiskPath: "/vol/a.img", GuestPath: "/data", Label: "a"},
			{DiskPath: "/vol/b.img", GuestPath: "/logs", Label: "b"},
		},
	})
	require.Equal(t, "a:/data\nb:/logs", fwCfgMounts(args))
}

func TestQEMUMountArgs_SkipsUnlabeledOrUnmounted(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
		Volumes: []VolumeMount{
			{DiskPath: "/vol/a.img", GuestPath: "/data"}, // no label
			{DiskPath: "/vol/b.img", Label: "b"},         // no guest path
		},
	})
	require.Empty(t, fwCfgMounts(args), "no fw_cfg mounts arg when no volume is both labeled and mounted")
}

func TestQEMUMountArgs_NoneWhenNoVolumes(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{ImagePath: "disk.img", Memory: "256M"})
	require.Empty(t, fwCfgMounts(args))
}

func TestFCBootArgs_InjectsMounts(t *testing.T) {
	args := buildFCBootArgs(Config{
		Volumes: []VolumeMount{
			{DiskPath: "/vol/pg.img", GuestPath: "/var/lib/postgresql/data", Label: "pgdata"},
			{DiskPath: "/vol/x.img", GuestPath: "/data"}, // unlabeled: skipped
		},
	})
	require.Contains(t, args, "mounts.pgdata=/var/lib/postgresql/data")
	require.NotContains(t, args, "mounts.=/data")
}
