package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestAsyncProfilerChecksum(t *testing.T) {
	sum := sha256.Sum256(AsyncProfiler)
	if got := hex.EncodeToString(sum[:]); got != AsyncProfilerSHA256 {
		t.Fatalf("libasyncProfiler.so SHA-256 %s, want %s", got, AsyncProfilerSHA256)
	}
}

// TestNoticeDocumentsLibrary: NOTICE names the bundled version and checksum,
// so a replaced library can't go unnoticed there.
func TestNoticeDocumentsLibrary(t *testing.T) {
	b, err := os.ReadFile("NOTICE")
	if err != nil {
		t.Fatal(err)
	}
	notice := strings.Join(strings.Fields(string(b)), " ")
	for _, want := range []string{
		"async-profiler " + AsyncProfilerVersion + " for linux-x64",
		"async-profiler-" + AsyncProfilerVersion + "-linux-x64/lib/libasyncProfiler.so in it has the SHA-256 " + AsyncProfilerSHA256,
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("NOTICE does not say %q", want)
		}
	}
}
