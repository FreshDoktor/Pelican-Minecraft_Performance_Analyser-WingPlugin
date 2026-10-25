//go:build !linux

package profile

import "os"

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }

func freeBytes(string) (int64, bool) { return 0, false }

func ownedByRoot(os.FileInfo) bool { return true }
