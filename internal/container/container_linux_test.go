package container

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindJavaTmpSymlink: a /tmp that is a symlink (custom image) leads
// nowhere, an absolute one would point into the host.
func TestFindJavaTmpSymlink(t *testing.T) {
	root := fakeProc(t, map[int][3]string{2000: {"1", "java", "26"}})
	host := t.TempDir()
	write(t, filepath.Join(host, "hsperfdata_x", "26"), "host file")
	tmp := filepath.Join(root, "2000", "root", "tmp")
	os.Remove(tmp)
	if err := os.Symlink(host, tmp); err != nil {
		t.Fatal(err)
	}
	j, err := Proc{Root: root}.FindJava(2000)
	if err != nil || !j.Hsperf.IsZero() {
		t.Errorf("symlinked /tmp must be ignored: %+v %v", j, err)
	}
}
