package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestAsyncProfilerChecksum(t *testing.T) {
	sum := sha256.Sum256(AsyncProfiler)
	if got := hex.EncodeToString(sum[:]); got != AsyncProfilerSHA256 {
		t.Fatalf("libasyncProfiler.so SHA-256 %s, want %s", got, AsyncProfilerSHA256)
	}
}
