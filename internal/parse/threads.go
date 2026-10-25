package parse

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"
)

// ThreadDump is the parsed output of Thread.print -l.
type ThreadDump struct {
	Timestamp string // as printed by the JVM, container local time
	VM        string // e.g. "OpenJDK 64-Bit Server VM (25.0.4.1+1-LTS mixed mode, sharing)"
	Threads   []Thread
	Deadlocks []Deadlock
	JNIGlobal *int64
	JNIWeak   *int64
}

// Thread is one thread of a dump.
type Thread struct {
	Name          string
	JavaID        *int64
	OSID          *int64
	Daemon        bool
	Priority      *int64
	OSPriority    *int64
	CPUMs         *float64
	ElapsedMs     *float64
	OSState       string
	State         string
	StateDetail   string
	VMThread      bool
	Stack         []Frame
	Synchronizers []Lock
	Info          []string
}

// Frame is one "at ..." line with the lock lines below it.
type Frame struct {
	Method string
	Module string
	File   string
	Line   *int64
	Native bool
	Locks  []Lock
}

// Lock is a lock line ("- locked <0x..> (a java.lang.Object)").
type Lock struct {
	Type    string // waiting_on, locked, parking, waiting_to_lock, waiting_to_relock, eliminated, owned
	Address string
	Class   string
}

// Deadlock is one "Found one Java-level deadlock" block.
type Deadlock struct {
	Threads []DeadlockThread
}

// DeadlockThread is one thread of a deadlock cycle.
type DeadlockThread struct {
	Name       string
	WaitingFor string
	HeldBy     string
}

var (
	reHeader = regexp.MustCompile(`^"(.*)" (.*)$`)
	reJavaID = regexp.MustCompile(`^#(\d+)(?: \[(\d+)\])?`)
	reKV     = regexp.MustCompile(`\b(prio|os_prio|cpu|elapsed|tid|nid)=(\S+)`)
	reLock   = regexp.MustCompile(`^- (waiting on|locked|parking to wait for|waiting to lock|waiting to re-lock in wait\(\)|eliminated) +(<[^>]*>)?\s*(?:\(a (.*)\))?`)
	reSync   = regexp.MustCompile(`^- (<[^>]*>) \(a (.*)\)$`)
	reJNI    = regexp.MustCompile(`^JNI global refs: (\d+), weak refs: (\d+)`)
	reDLWait = regexp.MustCompile(`^\s+(waiting to lock .*|waiting for .*|waiting on .*),?$`)
	reDLHeld = regexp.MustCompile(`^\s+which is held by "(.*)"`)
	reDLName = regexp.MustCompile(`^"(.*)":$`)
)

var lockTypes = map[string]string{
	"waiting on":                   "waiting_on",
	"locked":                       "locked",
	"parking to wait for":          "parking",
	"waiting to lock":              "waiting_to_lock",
	"waiting to re-lock in wait()": "waiting_to_relock",
	"eliminated":                   "eliminated",
}

// ParseThreadDump parses the text of Thread.print [-l].
func ParseThreadDump(text string) ThreadDump {
	var d ThreadDump
	var cur *Thread
	inSync := false
	inDeadlock := false
	var dl *Deadlock
	var dlt *DeadlockThread
	dlStacks := false // inside "Java stack information", which repeats the stacks

	flush := func() {
		if cur != nil {
			d.Threads = append(d.Threads, *cur)
			cur = nil
		}
		inSync = false
	}

	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \r")
		trim := strings.TrimSpace(line)

		if inDeadlock {
			switch {
			case strings.HasPrefix(trim, "Java stack information"):
				dlStacks, dlt = true, nil
			case strings.HasPrefix(trim, "Found ") && strings.Contains(trim, "deadlock"):
				dlStacks = false
				if strings.HasPrefix(trim, "Found one") {
					d.Deadlocks = append(d.Deadlocks, Deadlock{})
					dl = &d.Deadlocks[len(d.Deadlocks)-1]
				} else {
					inDeadlock = false
				}
			case dlStacks:
			case dl != nil && reDLName.MatchString(line):
				dl.Threads = append(dl.Threads, DeadlockThread{Name: reDLName.FindStringSubmatch(line)[1]})
				dlt = &dl.Threads[len(dl.Threads)-1]
			case dlt != nil && reDLWait.MatchString(line) && dlt.WaitingFor == "":
				dlt.WaitingFor = strings.TrimSuffix(strings.TrimSpace(line), ",")
			case dlt != nil && reDLHeld.MatchString(line):
				dlt.HeldBy = reDLHeld.FindStringSubmatch(line)[1]
			}
			continue
		}

		switch {
		case d.Timestamp == "" && d.VM == "" && len(trim) == 19 && trim[4] == '-' && trim[13] == ':':
			d.Timestamp = trim
			continue
		case strings.HasPrefix(trim, "Full thread dump "):
			d.VM = strings.TrimSuffix(strings.TrimPrefix(trim, "Full thread dump "), ":")
			continue
		case strings.HasPrefix(trim, "Found one Java-level deadlock"):
			flush()
			inDeadlock = true
			d.Deadlocks = append(d.Deadlocks, Deadlock{})
			dl = &d.Deadlocks[len(d.Deadlocks)-1]
			continue
		case reJNI.MatchString(trim):
			flush()
			m := reJNI.FindStringSubmatch(trim)
			d.JNIGlobal, d.JNIWeak = intPtr(m[1]), intPtr(m[2])
			continue
		}

		if m := reHeader.FindStringSubmatch(line); m != nil && !strings.HasPrefix(line, " ") {
			flush()
			cur = parseHeader(m[1], m[2])
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case trim == "":
		case strings.HasPrefix(trim, "java.lang.Thread.State: "):
			st := strings.TrimPrefix(trim, "java.lang.Thread.State: ")
			if s, detail, ok := strings.Cut(st, " ("); ok {
				cur.State, cur.StateDetail = s, strings.TrimSuffix(detail, ")")
			} else {
				cur.State = st
			}
		case strings.HasPrefix(trim, "at "):
			cur.Stack = append(cur.Stack, parseFrame(strings.TrimPrefix(trim, "at ")))
		case trim == "Locked ownable synchronizers:":
			inSync = true
		case inSync && strings.HasPrefix(trim, "- "):
			if m := reSync.FindStringSubmatch(trim); m != nil {
				cur.Synchronizers = append(cur.Synchronizers, Lock{Type: "owned", Address: strings.Trim(m[1], "<>"), Class: m[2]})
			}
		case strings.HasPrefix(trim, "- ") && len(cur.Stack) > 0:
			if m := reLock.FindStringSubmatch(trim); m != nil {
				l := Lock{Type: lockTypes[m[1]], Address: strings.Trim(m[2], "<>"), Class: m[3]}
				if strings.HasPrefix(l.Address, "owner is") || strings.Contains(l.Address, " ") {
					l.Address = ""
				}
				f := &cur.Stack[len(cur.Stack)-1]
				f.Locks = append(f.Locks, l)
			}
		default:
			cur.Info = append(cur.Info, trim)
		}
	}
	flush()
	return d
}

func parseHeader(name, rest string) *Thread {
	t := &Thread{Name: name}
	if m := reJavaID.FindStringSubmatch(rest); m != nil {
		t.JavaID = intPtr(m[1])
		rest = strings.TrimSpace(rest[len(m[0]):])
	} else {
		t.VMThread = true
	}
	if strings.HasPrefix(rest, "daemon ") {
		t.Daemon = true
	}
	for _, kv := range reKV.FindAllStringSubmatch(rest, -1) {
		switch kv[1] {
		case "prio":
			t.Priority = intPtr(kv[2])
		case "os_prio":
			t.OSPriority = intPtr(kv[2])
		case "cpu":
			t.CPUMs = durationMs(kv[2])
		case "elapsed":
			t.ElapsedMs = durationMs(kv[2])
		case "nid":
			if strings.HasPrefix(kv[2], "0x") {
				if n, err := strconv.ParseInt(kv[2][2:], 16, 64); err == nil {
					t.OSID = &n
				}
			} else {
				t.OSID = intPtr(kv[2])
			}
		}
	}
	// The OS state follows "nid=<n> " and precedes the optional "[0x...]".
	if i := strings.Index(rest, " nid="); i >= 0 {
		s := rest[i+5:]
		if j := strings.IndexByte(s, ' '); j >= 0 {
			s = s[j+1:]
		} else {
			s = ""
		}
		if j := strings.LastIndex(s, " ["); j >= 0 {
			s = s[:j]
		}
		t.OSState = strings.TrimSpace(s)
	}
	return t
}

func parseFrame(s string) Frame {
	f := Frame{Method: s}
	open := strings.LastIndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return f
	}
	f.Method = s[:open]
	loc := s[open+1 : len(s)-1]
	// HotSpot prints "(module@version/File.java:123)"; file names never contain '/'.
	if i := strings.IndexByte(loc, '/'); i >= 0 {
		f.Module, loc = loc[:i], loc[i+1:]
	}
	switch {
	case loc == "Native Method":
		f.Native = true
	case loc == "Unknown Source":
	default:
		file, line, ok := strings.Cut(loc, ":")
		f.File = file
		if ok {
			f.Line = intPtr(line)
		}
	}
	return f
}

func intPtr(s string) *int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// durationMs parses "656.31ms" or "19.43s".
func durationMs(s string) *float64 {
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "ms"):
		s = s[:len(s)-2]
	case strings.HasSuffix(s, "s"):
		s, mult = s[:len(s)-1], 1000
	default:
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	v := roundMs(f * mult)
	return &v
}
