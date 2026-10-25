//go:build !linux

package hsperf

import "os"

func openFile(path string) (*os.File, error) { return os.Open(path) }
