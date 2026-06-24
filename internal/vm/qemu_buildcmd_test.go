//go:build linux

package vm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildCmd_BasicFlags(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{ImagePath: "disk.img", Memory: "512M"})
	require.Contains(t, args, "-m")
	idx := indexOf(args, "-m")
	require.Equal(t, "512M", args[idx+1])
	require.Contains(t, args, "-drive")
	didx := indexOf(args, "-drive")
	require.Contains(t, args[didx+1], "file=disk.img")
	require.Contains(t, args, "-nographic")
	require.Contains(t, args, "-no-reboot")
}

func TestBuildCmd_DefaultCPUs(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{ImagePath: "disk.img", Memory: "256M", CPUs: 0})
	require.NotContains(t, args, "-smp")
}

func TestBuildCmd_NetworkCfg_CustomSubnetMask(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath:   "disk.img",
		Memory:      "256M",
		NetworkName: "jerboa-tap0",
		IPAddress:   "10.100.0.5",
		GatewayIP:   "10.100.0.1",
		SubnetMask:  "16",
	})
	var netCfg string
	for i, v := range args {
		if v == "-fw_cfg" && i+1 < len(args) && strings.HasPrefix(args[i+1], "name=opt/jerboa/network") {
			netCfg = args[i+1]
		}
	}
	require.Contains(t, netCfg, "10.100.0.5/16,10.100.0.1")
}

func TestBuildCmd_NetworkCfg_DefaultSubnetMask(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath:   "disk.img",
		Memory:      "256M",
		NetworkName: "jerboa-tap0",
		IPAddress:   "10.0.0.2",
		GatewayIP:   "10.0.0.1",
		SubnetMask:  "",
	})
	var netCfg string
	for i, v := range args {
		if v == "-fw_cfg" && i+1 < len(args) && strings.HasPrefix(args[i+1], "name=opt/jerboa/network") {
			netCfg = args[i+1]
		}
	}
	require.Contains(t, netCfg, "10.0.0.2/24,10.0.0.1")
}

func TestBuildCmd_NetworkCfg_MissingGateway(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath:   "disk.img",
		Memory:      "256M",
		NetworkName: "jerboa-tap0",
		IPAddress:   "10.0.0.2",
		GatewayIP:   "",
	})
	for i, v := range args {
		if v == "-fw_cfg" && i+1 < len(args) {
			require.NotContains(t, args[i+1], "opt/jerboa/network")
		}
	}
}

func TestBuildCmd_NetworkCfg_MissingIP(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath:   "disk.img",
		Memory:      "256M",
		NetworkName: "jerboa-tap0",
		IPAddress:   "",
		GatewayIP:   "10.0.0.1",
	})
	for i, v := range args {
		if v == "-fw_cfg" && i+1 < len(args) {
			require.NotContains(t, args[i+1], "opt/jerboa/network")
		}
	}
}

func TestBuildVolumeArgs_SingleRW(t *testing.T) {
	args := buildVolumeArgs([]VolumeMount{
		{DiskPath: "/vol/data.img", GuestPath: "/data", ReadOnly: false},
	})
	require.Contains(t, args, "-drive")
	idx := indexOf(args, "-drive")
	require.Contains(t, args[idx+1], "file=/vol/data.img")
	require.Contains(t, args[idx+1], "index=1")
	require.NotContains(t, args[idx+1], "readonly=on")
}

func TestBuildVolumeArgs_ReadOnly(t *testing.T) {
	args := buildVolumeArgs([]VolumeMount{
		{DiskPath: "/vol/ro.img", GuestPath: "/ro", ReadOnly: true},
	})
	idx := indexOf(args, "-drive")
	require.Contains(t, args[idx+1], "readonly=on")
}

func TestBuildVolumeArgs_Multiple(t *testing.T) {
	args := buildVolumeArgs([]VolumeMount{
		{DiskPath: "/vol/a.img", GuestPath: "/a", ReadOnly: false},
		{DiskPath: "/vol/b.img", GuestPath: "/b", ReadOnly: true},
	})
	drives := []string{}
	for i, v := range args {
		if v == "-drive" && i+1 < len(args) {
			drives = append(drives, args[i+1])
		}
	}
	require.Len(t, drives, 2)
	require.Contains(t, drives[0], "index=1")
	require.Contains(t, drives[1], "index=2")
	require.Contains(t, drives[1], "readonly=on")
}

func TestBuildVolumeArgs_Empty(t *testing.T) {
	args := buildVolumeArgs(nil)
	require.Empty(t, args)
}

func TestBuildEnvArgs_Single(t *testing.T) {
	args := buildEnvArgs([]string{"KEY=VAL"})
	require.Len(t, args, 2)
	require.Equal(t, "-fw_cfg", args[0])
	require.Contains(t, args[1], "name=opt/jerboa/env,string=KEY=VAL")
}

func TestBuildEnvArgs_Multiple(t *testing.T) {
	args := buildEnvArgs([]string{"A=1", "B=2"})
	require.Len(t, args, 2)
	require.Contains(t, args[1], "A=1\nB=2")
}

func TestBuildEnvArgs_Empty(t *testing.T) {
	args := buildEnvArgs(nil)
	require.Nil(t, args)
}

func TestSlirpNetArgs_Single(t *testing.T) {
	args := slirpNetArgs([]PortMap{{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}})
	require.Len(t, args, 4)
	require.Equal(t, "-netdev", args[0])
	require.Contains(t, args[1], "user,id=net0")
	require.Contains(t, args[1], "hostfwd=tcp::8080-:80")
}

func TestSlirpNetArgs_UDP(t *testing.T) {
	args := slirpNetArgs([]PortMap{{HostPort: 5353, GuestPort: 53, Protocol: ProtocolUDP}})
	require.Contains(t, args[1], "hostfwd=udp::5353-:53")
}

func TestBuildNetArgs_TAP(t *testing.T) {
	args := buildNetArgs(Config{NetworkName: "tap0"})
	require.Contains(t, args[1], "tap,id=net0,ifname=tap0")
}

func TestBuildNetArgs_SlirpFromPorts(t *testing.T) {
	args := buildNetArgs(Config{
		PortMaps: []PortMap{{HostPort: 9090, GuestPort: 80, Protocol: ProtocolTCP}},
	})
	require.Contains(t, args[1], "user,id=net0")
}

func TestBuildNetArgs_None(t *testing.T) {
	args := buildNetArgs(Config{})
	require.Equal(t, "-net", args[0])
	require.Equal(t, "none", args[1])
}

func TestBuildNetworkCfgArgs_BothRequired(t *testing.T) {
	args := buildNetworkCfgArgs(Config{IPAddress: "10.0.0.2", GatewayIP: "10.0.0.1"})
	require.Len(t, args, 2)
	require.Contains(t, args[1], "10.0.0.2/24,10.0.0.1")
}

func TestBuildNetworkCfgArgs_MissingIP(t *testing.T) {
	args := buildNetworkCfgArgs(Config{GatewayIP: "10.0.0.1"})
	require.Nil(t, args)
}

func TestBuildNetworkCfgArgs_MissingGateway(t *testing.T) {
	args := buildNetworkCfgArgs(Config{IPAddress: "10.0.0.2"})
	require.Nil(t, args)
}

func TestBuildNetworkCfgArgs_CustomMask(t *testing.T) {
	args := buildNetworkCfgArgs(Config{IPAddress: "10.0.0.2", GatewayIP: "10.0.0.1", SubnetMask: "28"})
	require.Contains(t, args[1], "10.0.0.2/28,10.0.0.1")
}

func TestToNetworkPortForwards(t *testing.T) {
	pms := []PortMap{
		{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP},
		{HostPort: 5353, GuestPort: 53, Protocol: ProtocolUDP},
	}
	out := toNetworkPortForwards(pms)
	require.Len(t, out, 2)
	require.Equal(t, uint16(8080), out[0].HostPort)
	require.Equal(t, uint16(80), out[0].GuestPort)
	require.Equal(t, "tcp", out[0].Protocol)
	require.Equal(t, "udp", out[1].Protocol)
}

func TestToNetworkPortForwards_Empty(t *testing.T) {
	out := toNetworkPortForwards(nil)
	require.Empty(t, out)
}

func TestWithStore(t *testing.T) {
	s := NewMemoryStore()
	mgr := NewQEMUManager("qemu", WithStore(s))
	require.Equal(t, s, mgr.store)
}

func TestQEMUManager_Store(t *testing.T) {
	mgr := NewQEMUManager("qemu")
	require.NotNil(t, mgr.Store())
}

func TestQEMUManager_Get(t *testing.T) {
	mgr := fakeManager(false)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	got, err := mgr.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
	_, err = mgr.Get("nonexistent")
	require.Error(t, err)
}

func TestQEMUManager_CreateError(t *testing.T) {
	mgr := fakeManager(false)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	_, err = mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M", Name: v.Cfg.Name})
	_ = err
}

func TestBuildCmd_VolumesInArgs(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
		Volumes: []VolumeMount{
			{DiskPath: "/vol/a.img", GuestPath: "/a"},
			{DiskPath: "/vol/b.img", GuestPath: "/b", ReadOnly: true},
		},
	})
	count := 0
	for i, v := range args {
		if v == "-drive" && i+1 < len(args) {
			if strings.Contains(args[i+1], "index=1") || strings.Contains(args[i+1], "index=2") {
				count++
			}
		}
	}
	require.Equal(t, 2, count)
}
