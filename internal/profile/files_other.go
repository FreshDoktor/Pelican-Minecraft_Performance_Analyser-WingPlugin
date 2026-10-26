//go:build !linux

package profile

import (
	"os"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
)

func freeBytes(ctmp.Dir) (int64, bool) { return 0, false }

func ownedByRoot(os.FileInfo) bool { return true }
