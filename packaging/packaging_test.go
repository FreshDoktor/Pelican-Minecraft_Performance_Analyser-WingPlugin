// Package packaging holds the files to install the bridge on a node; the test
// keeps the copy of the systemd unit inside install.sh in sync with the file.
package packaging

import (
	"os"
	"strings"
	"testing"
)

func TestInstallScriptUnitMatchesFile(t *testing.T) {
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile("mpa-bridge.service")
	if err != nil {
		t.Fatal(err)
	}
	const start = "cat >\"$UNIT_PATH\" <<'UNIT'\n"
	_, rest, ok := strings.Cut(string(script), start)
	if !ok {
		t.Fatal("unit heredoc not found in install.sh")
	}
	embedded, _, ok := strings.Cut(rest, "\nUNIT\n")
	if !ok {
		t.Fatal("end of the unit heredoc not found in install.sh")
	}
	if embedded+"\n" != string(unit) {
		t.Errorf("the unit in install.sh differs from mpa-bridge.service")
	}
}
