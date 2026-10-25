// Package assets embeds async-profiler (Apache 2.0, see NOTICE).
package assets

import _ "embed"

// AsyncProfiler is libasyncProfiler.so 4.1 for linux-x64, unmodified from the
// official release.
//
//go:embed libasyncProfiler.so
var AsyncProfiler []byte

// AsyncProfilerSHA256 is the SHA-256 of AsyncProfiler (checked by a test).
const AsyncProfilerSHA256 = "8d16556469adc3fe6661f0c023bfc2081342c930796b1d68ae1cd8248bef4c28"

// AsyncProfilerVersion is the bundled release.
const AsyncProfilerVersion = "4.1"
