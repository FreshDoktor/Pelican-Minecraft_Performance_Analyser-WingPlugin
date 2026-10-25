// Package parse turns JVM text output (startup arguments, thread dumps,
// class histograms, collapsed stacks) into structured data.
package parse

import (
	"strconv"
	"strings"
)

// VMArgs holds the settings the API reports from java.rt.vmArgs.
// hsperfdata joins all arguments with spaces, so arguments containing spaces
// cannot be recovered exactly; none of the parsed ones can contain spaces.
type VMArgs struct {
	Xms                  *int64
	Xmx                  *int64
	Xss                  *int64
	MaxMetaspaceSize     *int64
	MaxRAMPercentage     *float64
	InitialRAMPercentage *float64
	GC                   *string // explicitly selected collector (serial, parallel, g1, zgc, shenandoah)
	Xrs                  bool
	DisableAttach        bool
}

// ParseVMArgs parses a java.rt.vmArgs string. Later arguments win, like in the JVM.
func ParseVMArgs(s string) VMArgs {
	var a VMArgs
	for _, f := range strings.Fields(s) {
		switch {
		case strings.HasPrefix(f, "-Xmx"):
			a.Xmx = sizePtr(f[4:])
		case strings.HasPrefix(f, "-Xms"):
			a.Xms = sizePtr(f[4:])
		case strings.HasPrefix(f, "-Xss"):
			a.Xss = sizePtr(f[4:])
		case f == "-Xrs":
			a.Xrs = true
		case strings.HasPrefix(f, "-XX:"):
			a.xx(f[4:])
		}
	}
	return a
}

func (a *VMArgs) xx(f string) {
	if name, val, ok := strings.Cut(f, "="); ok {
		switch name {
		case "MaxHeapSize":
			a.Xmx = sizePtr(val)
		case "InitialHeapSize":
			a.Xms = sizePtr(val)
		case "ThreadStackSize":
			// Given in KiB.
			if n := sizePtr(val); n != nil {
				v := *n * 1024
				a.Xss = &v
			}
		case "MaxMetaspaceSize":
			a.MaxMetaspaceSize = sizePtr(val)
		case "MaxRAMPercentage":
			a.MaxRAMPercentage = floatPtr(val)
		case "InitialRAMPercentage":
			a.InitialRAMPercentage = floatPtr(val)
		}
		return
	}
	if len(f) < 2 || (f[0] != '+' && f[0] != '-') {
		return
	}
	on := f[0] == '+'
	gcs := map[string]string{
		"UseSerialGC": "serial", "UseParallelGC": "parallel", "UseG1GC": "g1",
		"UseZGC": "zgc", "UseShenandoahGC": "shenandoah",
	}
	if gc, ok := gcs[f[1:]]; ok && on {
		a.GC = &gc
	}
	if f[1:] == "DisableAttachMechanism" {
		a.DisableAttach = on
	}
	if f[1:] == "ReduceSignalUsage" {
		a.Xrs = on
	}
}

// ParseSize parses a JVM size ("1907M", "2g", "524288") into bytes.
func ParseSize(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult = 1 << 10
	case 'm', 'M':
		mult = 1 << 20
	case 'g', 'G':
		mult = 1 << 30
	case 't', 'T':
		mult = 1 << 40
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n * mult, true
}

func sizePtr(s string) *int64 {
	if n, ok := ParseSize(s); ok {
		return &n
	}
	return nil
}

func floatPtr(s string) *float64 {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return &f
	}
	return nil
}
