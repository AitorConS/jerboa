//go:build linux || (darwin && arm64)

package vm

import (
	"os"
	"strings"
	"testing"
)

func TestBuildVolumeArgsEscapesCommas(t *testing.T) {
	args := buildVolumeArgs(Config{Volumes: []VolumeMount{{DiskPath: "/v/a,format=qcow2,file.driver=x"}}})
	got := strings.Join(args, " ")
	if !strings.Contains(got, "file=/v/a,,format=qcow2,,file.driver=x,format=raw") {
		t.Fatalf("comma not escaped: %s", got)
	}
}

func TestCheckPrivateDirRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	link := dir + "/link"
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if checkPrivateDir(link) == nil {
		t.Fatal("symlink accepted as runtime dir")
	}
	if err := checkPrivateDir(dir); err != nil {
		t.Fatalf("private dir rejected: %v", err)
	}
}
