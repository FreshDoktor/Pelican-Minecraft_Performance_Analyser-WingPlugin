package profile

import (
	"os"
	"syscall"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
)

// freeBytes returns the space available to unprivileged users below dir.
// The JVM writes the profile as the container user, so root reserves do not count.
func freeBytes(dir ctmp.Dir) (int64, bool) {
	n, err := dir.Free()
	return n, err == nil
}

func ownedByRoot(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == 0
}
