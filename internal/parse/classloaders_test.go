package parse

import (
	"os"
	"testing"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The fixtures are from JDK 25.0.5 with a MinecraftServer loaded by a
// URLClassLoader without parent (classloaders-url.txt) and by a loader that
// gives only java.*, jdk.* and sun.* to the platform loader, like Cleanroom's
// Foundation (classloaders-isolated.txt).
func TestFindsBootstrapClasses(t *testing.T) {
	const mc = "net.minecraft.server.MinecraftServer"
	url := readTestdata(t, "classloaders-url.txt")
	isolated := readTestdata(t, "classloaders-isolated.txt")
	if got := ParseClassLoaders(isolated); len(got) != 4 || got["0x00007e5d18141910"] != BootstrapLoader ||
		got["0x00007e5d18151500"] != "jdk.internal.loader.ClassLoaders$PlatformClassLoader" ||
		got["0x00007e5d1817cf90"] != "jdk.internal.loader.ClassLoaders$AppClassLoader" ||
		got["0x00007e5d18192d50"] != "l.Main$Isolated" {
		t.Errorf("loaders %v", got)
	}

	cases := []struct {
		name, hierarchy, loaders string
		want                     bool
	}{
		{"url", "java.lang.Object/null\n|--net.minecraft.server.MinecraftServer/0x000076b05018b7c0\n", url, true},
		{"isolated", "java.lang.Object/null\n|--net.minecraft.server.MinecraftServer/0x00007e5d18192d50\n", isolated, false},
		{"app", "java.lang.Object/null\r\n|--net.minecraft.server.MinecraftServer/0x00007e5d1817cf90\r\n", isolated, true},
		{"bootstrap", "java.lang.Object/null\n|--net.minecraft.server.MinecraftServer/null\n", isolated, true},
		// Loaded twice: every loader must find them.
		{"two loaders", "java.lang.Object/null\n|--net.minecraft.server.MinecraftServer/0x00007e5d1817cf90\n" +
			"|--net.minecraft.server.MinecraftServer/0x00007e5d18192d50\n", isolated, false},
		// A loader missing in VM.classloaders (folded) is unknown.
		{"unknown loader", "java.lang.Object/null\n|--net.minecraft.server.MinecraftServer/0x0000000000000001\n", isolated, false},
		{"subclass only", "java.lang.Object/null\n|--net.minecraft.server.MinecraftServer$Dedicated/0x00007e5d1817cf90\n", isolated, false},
		{"not loaded", "java.lang.Object/null\n", isolated, false},
		// Java 8 has neither command.
		{"no output", "", "", false},
	}
	for _, c := range cases {
		if got := FindsBootstrapClasses(c.hierarchy, c.loaders, mc); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseClassLoadersNamed(t *testing.T) {
	got := ParseClassLoaders(`+-- <bootstrap>
      |           Loader Data: 0x01
      +-- "app", jdk.internal.loader.ClassLoaders$AppClassLoader
      |                 +-- jdk.internal.reflect.DelegatingClassLoader (+ 3 more)
      |                       Loader Data: 0x03
`)
	if len(got) != 2 || got["0x01"] != BootstrapLoader || got["0x03"] != "jdk.internal.reflect.DelegatingClassLoader" {
		t.Errorf("%v", got)
	}
}
