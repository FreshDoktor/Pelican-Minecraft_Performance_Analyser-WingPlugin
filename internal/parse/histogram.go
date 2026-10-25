package parse

import (
	"bufio"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Histogram is the parsed output of GC.class_histogram.
type Histogram struct {
	Classes        []HistogramClass
	TotalInstances int64
	TotalBytes     int64
}

// HistogramClass is one line of a class histogram.
type HistogramClass struct {
	Rank        int64
	Name        string // as printed: "[B", "[Ljava.lang.Object;", "net.minecraft.Foo"
	Module      string // "java.base@25.0.4.1", only for JDK classes
	ElementType string // element type for arrays ("byte", "java.lang.Object"), else Name
	ArrayDepth  int
	Instances   int64
	Bytes       int64
}

var (
	reHistLine  = regexp.MustCompile(`^\s*(\d+):\s+(\d+)\s+(\d+)\s+(\S+)(?:\s+\((.*)\))?\s*$`)
	reHistTotal = regexp.MustCompile(`^Total\s+(\d+)\s+(\d+)\s*$`)
)

var primitives = map[byte]string{
	'B': "byte", 'C': "char", 'D': "double", 'F': "float",
	'I': "int", 'J': "long", 'S': "short", 'Z': "boolean",
}

// ParseHistogram parses the text of GC.class_histogram [-all].
func ParseHistogram(text string) Histogram {
	var h Histogram
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := reHistLine.FindStringSubmatch(line); m != nil {
			c := HistogramClass{Name: m[4], Module: m[5]}
			c.Rank, _ = strconv.ParseInt(m[1], 10, 64)
			c.Instances, _ = strconv.ParseInt(m[2], 10, 64)
			c.Bytes, _ = strconv.ParseInt(m[3], 10, 64)
			c.ElementType, c.ArrayDepth = ElementType(c.Name)
			h.Classes = append(h.Classes, c)
			continue
		}
		if m := reHistTotal.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			h.TotalInstances, _ = strconv.ParseInt(m[1], 10, 64)
			h.TotalBytes, _ = strconv.ParseInt(m[2], 10, 64)
		}
	}
	return h
}

// ElementType strips array brackets from a JVM class name.
func ElementType(name string) (string, int) {
	depth := 0
	for depth < len(name) && name[depth] == '[' {
		depth++
	}
	if depth == 0 {
		return name, 0
	}
	rest := name[depth:]
	if len(rest) == 1 {
		if p, ok := primitives[rest[0]]; ok {
			return p, depth
		}
	}
	if strings.HasPrefix(rest, "L") && strings.HasSuffix(rest, ";") {
		return rest[1 : len(rest)-1], depth
	}
	return rest, depth
}

func roundMs(f float64) float64 { return math.Round(f*1000) / 1000 }
