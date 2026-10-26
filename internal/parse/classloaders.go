package parse

import "strings"

// BootstrapLoader is the name of the bootstrap class loader in VM.classloaders.
const BootstrapLoader = "<bootstrap>"

// ClassLoaderData returns the class loader data (the address VM.classloaders
// verbose prints as "Loader Data") of every class named class in the output
// of "VM.class_hierarchy <class>" (JDK 9+), e.g.
// "|--net.minecraft.server.MinecraftServer/0x00007efc3818bd60". A class of the
// bootstrap loader has "null". class is in Java form (dots).
func ClassLoaderData(hierarchy, class string) []string {
	var cld []string
	for _, line := range strings.Split(hierarchy, "\n") {
		line = strings.TrimLeft(strings.TrimRight(line, "\r "), "|- ")
		rest, ok := strings.CutPrefix(line, class+"/")
		if !ok {
			continue
		}
		if i := strings.IndexByte(rest, ' '); i >= 0 {
			rest = rest[:i]
		}
		if rest != "" {
			cld = append(cld, rest)
		}
	}
	return cld
}

// ParseClassLoaders parses the output of "VM.classloaders verbose" (JDK 11+):
// class loader data → class of the loader, BootstrapLoader for the bootstrap
// loader. A loader line is "+-- <bootstrap>", "+-- java.net.URLClassLoader" or
// with a name "+-- \"app\", jdk.internal.loader.ClassLoaders$AppClassLoader";
// its "Loader Data: 0x…" follows before the next loader line.
func ParseClassLoaders(text string) map[string]string {
	loaders := map[string]string{}
	cur := ""
	for _, line := range strings.Split(text, "\n") {
		if _, rest, ok := strings.Cut(line, "+-- "); ok {
			if strings.HasPrefix(rest, `"`) {
				if _, after, ok := strings.Cut(rest, `", `); ok {
					rest = after
				}
			}
			cur, _, _ = strings.Cut(strings.TrimSpace(rest), " ")
			continue
		}
		if _, rest, ok := strings.Cut(line, "Loader Data:"); ok && cur != "" {
			if f := strings.Fields(rest); len(f) > 0 {
				loaders[f[0]] = cur
			}
		}
	}
	return loaders
}

// bootstrapDelegating are class loaders that ask the bootstrap loader for a
// class they don't have themselves. java.net.URLClassLoader is the loader of
// the server jar in the bundlers of vanilla and Paper.
var bootstrapDelegating = map[string]bool{
	BootstrapLoader: true,
	"jdk.internal.loader.ClassLoaders$PlatformClassLoader": true,
	"jdk.internal.loader.ClassLoaders$AppClassLoader":      true,
	"java.net.URLClassLoader":                              true,
}

// FindsBootstrapClasses tells whether the class named class (Java form) is
// loaded and all its loaders find classes that are only in the bootstrap
// loader. hierarchy is the output of "VM.class_hierarchy <class>", loaders
// that of "VM.classloaders verbose". Unknown loaders don't: they may load
// everything but java.* themselves (Cleanroom's Foundation).
func FindsBootstrapClasses(hierarchy, loaders, class string) bool {
	cld := ClassLoaderData(hierarchy, class)
	if len(cld) == 0 {
		return false
	}
	byCLD := ParseClassLoaders(loaders)
	for _, c := range cld {
		name := byCLD[c]
		if c == "null" {
			name = BootstrapLoader
		}
		if !bootstrapDelegating[name] {
			return false
		}
	}
	return true
}
