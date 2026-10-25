package parse

import (
	"bufio"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ProfileThread groups the collapsed stacks of one thread.
type ProfileThread struct {
	Name   string
	TID    *int64
	Count  int64
	Stacks []ProfileStack
}

// ProfileStack is one collapsed stack (root first, frames separated by ';').
type ProfileStack struct {
	Stack string
	Count int64
}

var reThreadFrame = regexp.MustCompile(`^\[(.*?)(?: tid=(\d+))?\]$`)

// ParseCollapsed parses async-profiler collapsed output recorded with -t
// (threads split). Stacks without a thread frame are grouped under the name "".
// Threads are ordered by count (descending), stacks likewise.
func ParseCollapsed(text string) []ProfileThread {
	type key struct {
		name string
		tid  string
	}
	byThread := map[key]*ProfileThread{}
	stackIdx := map[key]map[string]int{}
	var order []key

	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		sp := strings.LastIndexByte(line, ' ')
		if sp <= 0 {
			continue
		}
		n, err := strconv.ParseInt(line[sp+1:], 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		stack := line[:sp]
		k := key{}
		first, rest, _ := strings.Cut(stack, ";")
		if m := reThreadFrame.FindStringSubmatch(first); m != nil {
			k = key{m[1], m[2]}
			stack = rest
		}
		t, ok := byThread[k]
		if !ok {
			t = &ProfileThread{Name: k.name}
			if k.tid != "" {
				t.TID = intPtr(k.tid)
			}
			byThread[k] = t
			stackIdx[k] = map[string]int{}
			order = append(order, k)
		}
		t.Count += n
		if i, ok := stackIdx[k][stack]; ok {
			t.Stacks[i].Count += n
		} else {
			stackIdx[k][stack] = len(t.Stacks)
			t.Stacks = append(t.Stacks, ProfileStack{Stack: stack, Count: n})
		}
	}
	out := make([]ProfileThread, 0, len(order))
	for _, k := range order {
		t := byThread[k]
		sort.SliceStable(t.Stacks, func(i, j int) bool { return t.Stacks[i].Count > t.Stacks[j].Count })
		out = append(out, *t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}
