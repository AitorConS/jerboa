// Package upgradetest restores, for tests, the on-disk state that released
// versions of Jerboa left behind, so the current code can be shown to still
// open it: the images, names, VMs, volumes, networks, compose project,
// credentials and settings of a user who upgrades.
//
// A fixture is recorded by scripts/upgrade/make-fixture.py, which runs the
// released jerboa and jerboad through a fixed scenario and packs their home
// directories (testdata/<version>-<os>-<arch>/state.tar.zst) together with
// what that version answered about them (fixture.json). Nothing in a fixture
// is written by hand. Record one for every release:
//
//	scripts/upgrade/make-fixture-docker.sh v0.60.0        # Linux, in a container
//	python3 scripts/upgrade/make-fixture.py --version v0.60.0 \
//	    --bin-dir ".../Jerboa Desktop.app/Contents/Resources/bin" \
//	    --out internal/upgradetest/testdata                 # macOS
//
// The state names absolute paths, and a compose project is identified by a
// hash of its path, so a fixture is restored where it was recorded: a fixed
// directory (Fixture.Root) that Restore creates for the calling test alone
// and removes afterwards. Tests of different packages take turns through a
// lock file; a fixture recorded on another operating system is skipped.
package upgradetest

import (
	"archive/tar"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

//go:embed testdata
var testdata embed.FS

// File is one entry of a fixture's state as it was recorded.
type File struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Dir    bool   `json:"dir"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Perm returns the recorded permission bits.
func (f File) Perm() fs.FileMode {
	mode, err := strconv.ParseUint(strings.TrimPrefix(f.Mode, "0o"), 8, 32)
	if err != nil {
		panic(fmt.Sprintf("upgradetest: mode %q of %s", f.Mode, f.Path))
	}
	return fs.FileMode(mode)
}

// VM is what the released `jerboa inspect` printed for a VM.
type VM struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	State         string            `json:"state"`
	Image         string            `json:"image"`
	Memory        string            `json:"memory"`
	CPUs          int               `json:"cpus"`
	Env           []string          `json:"env"`
	IPAddress     string            `json:"ip_address"`
	GatewayIP     string            `json:"gateway_ip"`
	RestartPolicy string            `json:"restart_policy"`
	Health        string            `json:"health"`
	CreatedAt     string            `json:"created_at"`
	Volumes       []json.RawMessage `json:"volumes"`
	Ports         []json.RawMessage `json:"ports"`
}

// ComposeState is the compose state file of the released version.
type ComposeState struct {
	Project         string            `json:"project"`
	Services        map[string]string `json:"services"`
	CreatedVolumes  []string          `json:"created_volumes"`
	CreatedNetworks []string          `json:"created_networks"`
}

// Expected is what the released version reported before it was stopped. The
// raw lists are the JSON its CLI printed (`jerboa images --output json` and
// so on), kept as printed so the same commands of the current CLI can be
// compared with them.
type Expected struct {
	Images    []map[string]any  `json:"images"`
	Volumes   []map[string]any  `json:"volumes"`
	Networks  []map[string]any  `json:"networks"`
	Snapshots []map[string]any  `json:"snapshots"`
	VMs       map[string]VM     `json:"vms"`
	ImageDirs []string          `json:"image_dirs"`
	Creds     map[string]string `json:"credentials"`
	// Signed reports whether probe:latest was signed with `jerboa sign`.
	Signed bool `json:"signed"`
	// ImageDigest is the disk probe:latest and probe:v1 name.
	ImageDigest string `json:"image_digest"`
	// ReplacedImages are disks a rebuild of their tag left without a name:
	// one a VM still boots from, and one nothing used (which the released
	// daemon had already deleted).
	ReplacedImages struct {
		InUse  string `json:"in_use"`
		Unused string `json:"unused"`
	} `json:"replaced_images"`
	Config struct {
		Hypervisor string `json:"hypervisor"`
		Endpoint   string `json:"endpoint"`
		Token      string `json:"token"`
	} `json:"config"`
	Compose struct {
		File      string           `json:"file"`
		StateFile string           `json:"state_file"`
		State     ComposeState     `json:"state"`
		PS        []map[string]any `json:"ps"`
	} `json:"compose"`
	// SQLite is the second home, whose daemon was moved to --vm-store sqlite.
	SQLite struct {
		VMs map[string]VM `json:"vms"`
		// RemovedButLeftOnDisk is a VM removed after the move: the released
		// version deleted its row and left its state.json in the file
		// registry.
		RemovedButLeftOnDisk string `json:"removed_but_left_on_disk"`
	} `json:"sqlite"`
}

// Fixture is the recorded state of one released version on one platform.
type Fixture struct {
	// Name is the directory under testdata: <version>-<os>-<arch>.
	Name       string `json:"-"`
	Version    string `json:"version"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Hypervisor string `json:"hypervisor"`
	// Root is where the state was recorded and is restored.
	Root          string   `json:"root"`
	ArchiveSHA256 string   `json:"archive_sha256"`
	Files         []File   `json:"files"`
	Expected      Expected `json:"expected"`
}

// Home is the home directory of the user of the default (file) VM store.
func (f *Fixture) Home() string { return filepath.Join(f.Root, "home") }

// Data is that user's ~/.jerboa.
func (f *Fixture) Data() string { return filepath.Join(f.Home(), ".jerboa") }

// SQLiteHome is the home directory whose daemon ran with --vm-store sqlite.
func (f *Fixture) SQLiteHome() string { return filepath.Join(f.Root, "home-sqlite") }

// Before reports whether the fixture is of a release older than version
// ("v0.56.0"), for what a later release changed on purpose.
func (f *Fixture) Before(version string) bool { return versionLess(f.Version, version) }

// VMNames returns the recorded VM names, sorted.
func (f *Fixture) VMNames() []string {
	names := make([]string, 0, len(f.Expected.VMs))
	for name := range f.Expected.VMs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// All returns every committed fixture, oldest version first.
func All(t testing.TB) []*Fixture {
	t.Helper()
	entries, err := testdata.ReadDir("testdata")
	if err != nil {
		t.Fatalf("upgradetest: %v", err)
	}
	var out []*Fixture
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := testdata.ReadFile(path.Join("testdata", e.Name(), "fixture.json"))
		if err != nil {
			t.Fatalf("upgradetest: %v", err)
		}
		f := &Fixture{Name: e.Name()}
		if err := json.Unmarshal(data, f); err != nil {
			t.Fatalf("upgradetest: %s: %v", e.Name(), err)
		}
		if e.Name() != f.Version+"-"+f.OS+"-"+f.Arch {
			t.Fatalf("upgradetest: %s holds the fixture of %s %s/%s", e.Name(), f.Version, f.OS, f.Arch)
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return versionLess(out[i].Version, out[j].Version) })
	return out
}

// Each runs test once per fixture recorded on this operating system, as a
// subtest named after it, each over a freshly restored copy. It fails when
// there is none for this system, unless the system is one fixtures cannot be
// recorded on.
func Each(t *testing.T, test func(t *testing.T, f *Fixture)) {
	t.Helper()
	ran := false
	for _, f := range All(t) {
		if f.OS != runtime.GOOS {
			continue
		}
		ran = true
		t.Run(f.Name, func(t *testing.T) {
			f.Restore(t)
			test(t, f)
		})
	}
	if ran {
		return
	}
	if runtime.GOOS == "linux" {
		t.Fatal("upgradetest: no fixture is committed for linux")
	}
	t.Skipf("no upgrade fixture is recorded for %s", runtime.GOOS)
}

func versionLess(a, b string) bool {
	parse := func(v string) (out [3]int) {
		for i, part := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
			out[i], _ = strconv.Atoi(part)
		}
		return out
	}
	pa, pb := parse(a), parse(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

// Restore puts the recorded state back under f.Root, exactly as it was left
// (content, permissions and modification times), for the calling test only:
// other tests wait until this one ends, when the directory is removed.
func (f *Fixture) Restore(t testing.TB) {
	t.Helper()
	if f.OS != runtime.GOOS {
		t.Skipf("fixture %s was recorded on %s", f.Name, f.OS)
	}
	unlock, err := lock(f.Root + ".lock")
	if err != nil {
		t.Fatalf("upgradetest: %v", err)
	}
	t.Cleanup(func() {
		// Read-only directories would keep RemoveAll from emptying them.
		_ = filepath.WalkDir(f.Root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700) //nolint:gosec // a directory of this test
			}
			return nil
		})
		_ = os.RemoveAll(f.Root)
		unlock()
	})
	if err := f.restore(); err != nil {
		t.Fatalf("upgradetest: restore %s: %v", f.Name, err)
	}
}

// Reset puts the recorded state back again, discarding what the test did to
// it. Only for a fixture this test restored.
func (f *Fixture) Reset(t testing.TB) {
	t.Helper()
	if err := f.restore(); err != nil {
		t.Fatalf("upgradetest: restore %s: %v", f.Name, err)
	}
}

func (f *Fixture) restore() error {
	// A run that was killed leaves the directory behind; anything else at
	// this well-known path is not ours to remove.
	if info, err := os.Lstat(f.Root); err == nil {
		if !info.IsDir() || !ownedByUs(info) {
			return fmt.Errorf("%s exists and is not a directory of this user; remove it", f.Root)
		}
		if err := os.RemoveAll(f.Root); err != nil {
			return fmt.Errorf("remove stale %s: %w", f.Root, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", f.Root, err)
	}
	if err := os.Mkdir(f.Root, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", f.Root, err)
	}
	archive, err := testdata.ReadFile(path.Join("testdata", f.Name, "state.tar.zst"))
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != f.ArchiveSHA256 {
		return errors.New("state.tar.zst is not the archive fixture.json was recorded with")
	}
	// The archive is compressed with a 128 MiB window, so that VM boot disks
	// cost little beside the image they were cloned from.
	dec, err := zstd.NewReader(strings.NewReader(string(archive)), zstd.WithDecoderMaxWindow(1<<27))
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer dec.Close()
	recorded := make(map[string]File, len(f.Files))
	for _, file := range f.Files {
		recorded[file.Path] = file
	}
	type dirTime struct {
		path string
		hdr  *tar.Header
	}
	var dirs []dirTime
	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		want, ok := recorded[name]
		if !ok || !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("archive entry %q is not in fixture.json", hdr.Name)
		}
		delete(recorded, name)
		target := filepath.Join(f.Root, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			// Writable until its content is in place.
			if err := os.Mkdir(target, 0o700); err != nil {
				return fmt.Errorf("restore %s: %w", hdr.Name, err)
			}
			dirs = append(dirs, dirTime{target, hdr})
		case tar.TypeReg:
			if err := restoreFile(target, tr, want); err != nil {
				return fmt.Errorf("restore %s: %w", hdr.Name, err)
			}
			if err := os.Chtimes(target, hdr.ModTime, hdr.ModTime); err != nil {
				return fmt.Errorf("restore %s: %w", hdr.Name, err)
			}
		default:
			// Jerboa's state is directories and regular files; the recorder
			// refuses anything else, and so does this.
			return fmt.Errorf("archive entry %q has an unexpected type", hdr.Name)
		}
	}
	if len(recorded) != 0 {
		return fmt.Errorf("the archive lacks %d entries of fixture.json", len(recorded))
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if err := os.Chmod(d.path, fs.FileMode(d.hdr.Mode).Perm()); err != nil { //nolint:gosec // the recorded mode
			return fmt.Errorf("restore %s: %w", d.hdr.Name, err)
		}
		if err := os.Chtimes(d.path, d.hdr.ModTime, d.hdr.ModTime); err != nil {
			return fmt.Errorf("restore %s: %w", d.hdr.Name, err)
		}
	}
	return nil
}

func restoreFile(target string, content io.Reader, want File) error {
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err //nolint:wrapcheck // the caller names the entry
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), content) //nolint:gosec // bounded by the embedded archive
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err //nolint:wrapcheck // the caller names the entry
	}
	if n != want.Size || hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
		return errors.New("content differs from what was recorded")
	}
	// Exactly the recorded bits, whatever the umask.
	return os.Chmod(target, want.Perm()) //nolint:wrapcheck // the caller names the entry
}
