//go:build linux || (darwin && arm64)

package apiserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/vm"
)

// Free space below which the daemon's filesystem is reported: builds, pulls
// and VM starts all need room for a disk image.
const (
	lowSpaceWarn  = 5 << 30
	lowSpaceError = 1 << 30
)

func (s *Server) handleSystemCheck(params json.RawMessage) (any, *api.RPCError) {
	var p api.SystemCheckParams
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
		}
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	return api.SystemCheckResult{Findings: s.systemCheck(p)}, nil
}

// checker collects findings; a check that finds nothing wrong reports ok, so
// the result says what was looked at and not only what is broken.
type checker struct {
	findings []api.SystemFinding
}

func (c *checker) add(check, severity, hint, format string, args ...any) {
	c.findings = append(c.findings, api.SystemFinding{
		Check: check, Severity: severity, Message: fmt.Sprintf(format, args...), Hint: hint,
	})
}

// group runs one check and reports ok with summary when it added nothing.
func (c *checker) group(check string, fn func() (summary string)) {
	before := len(c.findings)
	summary := fn()
	if len(c.findings) == before {
		c.add(check, api.SeverityOK, "", "%s", summary)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// humanBytes renders a size the way the CLI's tables do.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func (s *Server) systemCheck(p api.SystemCheckParams) []api.SystemFinding {
	c := &checker{}
	vms := s.vms()

	c.group("hypervisor", func() string {
		h := s.hypervisor
		if h.Name == "" {
			return "running"
		}
		if h.Name != "firecracker" && h.FirecrackerAvailable {
			c.add("hypervisor", api.SeverityInfo, "jerboa config set hypervisor auto, then restart the daemon",
				"running %s (%s), but this host can run Firecracker, which starts faster", h.Name, h.Reason)
			return ""
		}
		return fmt.Sprintf("%s (%s)", h.Name, h.Reason)
	})

	c.group("disk space", func() string {
		total, free := filesystemSpace(s.paths.DataDir)
		if total == 0 {
			return "not measured"
		}
		msg := fmt.Sprintf("%s free of %s in %s", humanBytes(free), humanBytes(total), s.paths.DataDir)
		switch {
		case free < lowSpaceError:
			c.add("disk space", api.SeverityError, "jerboa system df shows what takes the space; jerboa system prune frees it", "%s", "only "+msg)
		case free < lowSpaceWarn:
			c.add("disk space", api.SeverityWarning, "jerboa system df shows what takes the space; jerboa system prune frees it", "%s", "only "+msg)
		}
		return msg
	})

	s.checkImages(c, vms, p.Deep)
	s.checkVMs(c, vms)
	s.checkVolumes(c, vms)
	s.checkNetworks(c, vms)
	checkGuard(c, network.CheckGuard)
	s.checkSnapshots(c)
	s.checkTools(c)
	return c.findings
}

func (s *Server) checkImages(c *checker, vms []*vm.VM, deep bool) {
	if s.imgStore == nil {
		c.add("images", api.SeverityError, "check the daemon log for why the image store did not open", "the image store is unavailable")
		return
	}
	c.group("images", func() string {
		root := s.imgStore.Root()
		refs, err := s.imgStore.Refs()
		if err != nil {
			c.add("images", api.SeverityError, "", "the image index cannot be read: %v", err)
			return ""
		}
		if _, err := os.Stat(filepath.Join(root, "refs.json.corrupt")); err == nil {
			c.add("images", api.SeverityInfo, "jerboa system prune removes the damaged copy",
				"the image index was found damaged and was rebuilt from the stored images")
		}
		names := make([]string, 0, len(refs))
		for ref := range refs {
			names = append(names, ref)
		}
		sort.Strings(names)
		verified := map[string]error{}
		for _, ref := range names {
			digest := refs[ref]
			dir := filepath.Join(root, strings.TrimPrefix(digest, "sha256:"))
			if _, err := os.Stat(filepath.Join(dir, "disk.img")); err != nil {
				c.add("images", api.SeverityWarning, "jerboa system prune drops the name; build or pull the image again",
					"image %s names a disk that is no longer in the store", ref)
				continue
			}
			if _, _, err := s.imgStore.Resolve(ref); err != nil {
				c.add("images", api.SeverityError, "jerboa rmi "+ref+", then build or pull it again",
					"image %s cannot be read: %v", ref, err)
				continue
			}
			if !deep {
				continue
			}
			verr, done := verified[digest]
			if !done {
				verr = s.imgStore.VerifyDisk(digest)
				verified[digest] = verr
			}
			if verr != nil {
				c.add("images", api.SeverityError, "jerboa rmi "+ref+", then build or pull it again",
					"image %s is corrupt: its disk no longer matches its digest", ref)
			}
		}
		inUse := vm.ImageDigestsInUse(vms)
		if unref, err := s.imgStore.Unreferenced(func(d string) bool { return inUse[d] }); err == nil && len(unref) > 0 {
			c.add("images", api.SeverityInfo, "jerboa system prune", "%s without a name (left by rebuilds) can be reclaimed", plural(len(unref), "stored image", "stored images"))
		}
		summary := plural(len(refs), "image", "images") + " readable"
		if deep {
			summary = plural(len(refs), "image", "images") + " verified against their digests"
		}
		return summary
	})
}

func (s *Server) checkVMs(c *checker, vms []*vm.VM) {
	c.group("vms", func() string {
		for _, v := range vms {
			name := v.ID
			if v.Cfg.Name != "" {
				name = v.Cfg.Name
			}
			running := v.GetState() == vm.StateRunning
			if p := v.Cfg.ImagePath; p != "" && !running {
				if _, err := os.Stat(p); os.IsNotExist(err) {
					c.add("vms", api.SeverityWarning, "jerboa rm "+name+", then run it again from its image",
						"VM %s cannot start again: its image is no longer in the store", name)
				}
			}
			for _, m := range v.Cfg.Volumes {
				if _, err := os.Stat(m.DiskPath); os.IsNotExist(err) {
					c.add("vms", api.SeverityError, "jerboa rm "+name+"; the volume's data is gone unless it was backed up",
						"VM %s mounts a volume whose disk is missing (%s)", name, m.GuestPath)
				}
			}
		}
		if stray := s.strayVMRecords(vms, 0); len(stray) > 0 {
			c.add("vms", api.SeverityWarning, "jerboa system prune",
				"%s could not be read back and belong to no VM", plural(len(stray), "VM record", "VM records"))
		}
		if orphans := vm.OrphanBootDisks(s.paths.VMDisks, vms); len(orphans) > 0 {
			c.add("vms", api.SeverityInfo, "jerboa system prune", "%s left by VMs that are not running", plural(len(orphans), "boot disk", "boot disks"))
		}
		return plural(len(vms), "VM", "VMs") + " consistent with the store"
	})
}

func (s *Server) checkVolumes(c *checker, _ []*vm.VM) {
	if s.volStore == nil {
		return
	}
	c.group("volumes", func() string {
		vols, err := s.volStore.List()
		if err != nil {
			c.add("volumes", api.SeverityError, "", "the volume store cannot be read: %v", err)
			return ""
		}
		listed := make(map[string]bool, len(vols))
		for _, v := range vols {
			listed[v.ID] = true
			info, err := os.Stat(v.DiskPath)
			switch {
			case err != nil:
				c.add("volumes", api.SeverityError, "jerboa volume rm "+v.ID, "volume %s has no disk", v.ID)
			case info.Size() != v.SizeBytes && v.SizeBytes > 0 && info.Size() < v.SizeBytes:
				c.add("volumes", api.SeverityWarning, "", "volume %s is %s but its disk is only %s", v.ID, humanBytes(v.SizeBytes), humanBytes(info.Size()))
			}
		}
		entries, _ := os.ReadDir(s.volStore.Root())
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !listed[e.Name()] {
				c.add("volumes", api.SeverityWarning, "remove the directory "+filepath.Join(s.volStore.Root(), e.Name())+" if it holds nothing you need",
					"%s is in the volume store but is not a volume", e.Name())
			}
		}
		if staging := s.volStore.Staging(); len(staging) > 0 {
			c.add("volumes", api.SeverityInfo, "jerboa system prune", "%s left by interrupted volume creations, imports or removals", plural(len(staging), "staging directory", "staging directories"))
		}
		return plural(len(vols), "volume", "volumes") + " with their disks"
	})
}

func (s *Server) checkNetworks(c *checker, vms []*vm.VM) {
	if s.netStore == nil {
		return
	}
	c.group("networks", func() string {
		nets, err := s.netStore.List()
		if err != nil {
			c.add("networks", api.SeverityError, "", "the network store cannot be read: %v", err)
			return ""
		}
		known := make(map[string]bool, len(nets))
		for _, n := range nets {
			known[n.Name] = true
		}
		missing := map[string][]string{}
		for _, v := range vms {
			if n := v.Cfg.NetworkName; n != "" && !known[n] {
				name := v.ID
				if v.Cfg.Name != "" {
					name = v.Cfg.Name
				}
				missing[n] = append(missing[n], name)
			}
		}
		names := make([]string, 0, len(missing))
		for n := range missing {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c.add("networks", api.SeverityWarning, "jerboa network create "+n+", or remove the VMs",
				"network %s is gone but %s still attached to it: %s", n, plural(len(missing[n]), "VM is", "VMs are"), strings.Join(missing[n], ", "))
		}
		return plural(len(nets), "network", "networks") + " readable"
	})
}

// guardHint is what to do about a guest network guard that is not loaded.
const guardHint = "install nftables (sudo apt-get install nftables); the daemon loads the guard within seconds"

// checkGuard reports the guest network guard of a Linux daemon (WSL
// included); state is network.CheckGuard, a parameter for the tests.
func checkGuard(c *checker, state func() network.GuardState) {
	st := state()
	if !st.Configured {
		return
	}
	c.group("network isolation", func() string {
		switch {
		case st.Err != nil && st.Enforced:
			c.add("network isolation", api.SeverityError, guardHint,
				"the guest network guard is not loaded, so VMs on networks cannot start: %v", st.Err)
		case st.Err != nil && st.AllowUnguarded:
			c.add("network isolation", api.SeverityWarning, guardHint+", or unset "+network.AllowUnguardedEnv,
				"the guest network guard is not loaded and %s=1: guests on networks run without isolation: %v", network.AllowUnguardedEnv, st.Err)
		case st.Err != nil:
			c.add("network isolation", api.SeverityWarning, guardHint,
				"the guest network guard is not loaded: guests on networks run without isolation: %v", st.Err)
		case st.NoBridgeFamily:
			c.add("network isolation", api.SeverityInfo, "",
				"guest network guard loaded; this kernel has no nftables bridge family (WSL), so guests are not bound to their own MAC and IP address")
		}
		if st.Err == nil && st.HostAccess {
			c.add("network isolation", api.SeverityInfo, "unset JERBOA_GUEST_HOST_ACCESS and restart the daemon to keep guests off them",
				"guest network guard loaded; JERBOA_GUEST_HOST_ACCESS=1 lets guests open connections to the host and to private networks")
		}
		return "guest network guard loaded"
	})
}

func (s *Server) checkSnapshots(c *checker) {
	sm, ok := s.mgr.(snapshotManager)
	if !ok {
		return
	}
	c.group("snapshots", func() string {
		entries, err := sm.SnapshotList()
		if err != nil {
			c.add("snapshots", api.SeverityError, "", "the snapshot store cannot be read: %v", err)
			return ""
		}
		for _, e := range entries {
			if e.Err != nil {
				c.add("snapshots", api.SeverityError, "jerboa snapshot rm "+e.Info.Name, "snapshot %s cannot be used: %v", e.Info.Name, e.Err)
			}
		}
		return plural(len(entries), "snapshot", "snapshots") + " readable"
	})
}

func (s *Server) checkTools(c *checker) {
	if s.paths.Tools == "" {
		return
	}
	c.group("kernel", func() string {
		files, bytes := treeUsage(s.paths.Tools)
		if files == 0 {
			c.add("kernel", api.SeverityInfo, "", "no kernel toolset downloaded yet; the first build or run fetches it")
			return ""
		}
		return fmt.Sprintf("toolsets present (%s)", humanBytes(bytes))
	})
}
