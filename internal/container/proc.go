package container

import (
	"bufio"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
)

// ErrNoJava means no java process runs in the container.
var ErrNoJava = errors.New("container: no java process")

// JavaProcess is the JVM found in a container.
type JavaProcess struct {
	HostPID int
	NSPID   int       // PID inside the container (last value of NSpid)
	User    string    // from /tmp/hsperfdata_<user>; empty if no hsperfdata file
	Hsperf  ctmp.File // the hsperfdata file, zero if none
	Tmp     ctmp.Dir  // the container's /tmp, bound to the process (StartTime)
	// StartTime of HostPID (stat field 22) tells the JVM from a process that
	// got its PID after it ended.
	StartTime uint64
}

// AttachSocket is the attach socket (/tmp/.java_pid<ns-pid>).
func (j JavaProcess) AttachSocket() ctmp.File {
	return ctmp.File{Dir: j.Tmp, Name: ".java_pid" + strconv.Itoa(j.NSPID)}
}

// Proc reads a procfs mounted at Root ("/proc" in production, a fake tree in tests).
type Proc struct {
	Root string
}

// FindJava walks the descendants of initPID (breadth first) and returns the
// first java process that has an hsperfdata file, else the first java process.
func (p Proc) FindJava(initPID int) (JavaProcess, error) {
	children, err := p.childMap()
	if err != nil {
		return JavaProcess{}, err
	}
	var first *JavaProcess
	queue := []int{initPID}
	seen := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p.isJava(pid) {
			j, err := p.javaProcess(pid)
			if err == nil {
				if !j.Hsperf.IsZero() {
					return j, nil
				}
				if first == nil {
					first = &j
				}
			}
		}
		queue = append(queue, children[pid]...)
	}
	if first != nil {
		return *first, nil
	}
	return JavaProcess{}, ErrNoJava
}

// CPUTicks returns utime+stime of pid in clock ticks (USER_HZ).
func (p Proc) CPUTicks(pid int) (int64, error) {
	b, err := os.ReadFile(filepath.Join(p.Root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	f := statFields(string(b))
	if len(f) < 15 {
		return 0, errors.New("container: short stat")
	}
	u, _ := strconv.ParseInt(f[13], 10, 64)
	s, _ := strconv.ParseInt(f[14], 10, 64)
	return u + s, nil
}

// ErrNoThread means the process has no thread of that name.
var ErrNoThread = errors.New("container: no such thread")

// ThreadID returns the native id of the thread of pid named name (the lowest,
// if several are), from /proc/<pid>/task/<tid>/comm; Linux cuts the names to
// 15 bytes. The id is the one inside the container's PID namespace (last value
// of NSpid), as the JVM and async-profiler know it. named is false when the JVM gives its threads no native names
// (Java 8: every thread is "java"), then name can't be found this way.
func (p Proc) ThreadID(pid int, name string) (tid int, named bool, err error) {
	dir := filepath.Join(p.Root, strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false, err
	}
	if len(name) > 15 {
		name = name[:15]
	}
	for _, e := range entries {
		id, err := strconv.Atoi(e.Name())
		if err != nil || id == pid {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "comm"))
		if err != nil {
			continue
		}
		comm := strings.TrimSuffix(string(b), "\n")
		if comm != "java" {
			named = true
		}
		if comm == name && (tid == 0 || id < tid) {
			tid = id
		}
	}
	if tid == 0 {
		return 0, named, ErrNoThread
	}
	return p.nsID(filepath.Join(dir, strconv.Itoa(tid), "status"), tid), true, nil
}

// HostTID returns the host's id of the thread of pid that has the id nsTID in
// the container's PID namespace (the reverse of ThreadID's translation).
func (p Proc) HostTID(pid, nsTID int) (int, error) {
	dir := filepath.Join(p.Root, strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		id, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if p.nsID(filepath.Join(dir, e.Name(), "status"), id) == nsTID {
			return id, nil
		}
	}
	return 0, ErrNoThread
}

// SchedStat returns how long a thread ran on a CPU and how long it waited
// for one while runnable (CPU limit of the container included), in ns, from
// /proc/<pid>/task/<tid>/schedstat (tid as the host knows it).
func (p Proc) SchedStat(pid, tid int) (run, wait int64, err error) {
	b, err := os.ReadFile(filepath.Join(p.Root, strconv.Itoa(pid), "task", strconv.Itoa(tid), "schedstat"))
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, 0, errors.New("container: short schedstat")
	}
	if run, err = strconv.ParseInt(f[0], 10, 64); err != nil {
		return 0, 0, err
	}
	if wait, err = strconv.ParseInt(f[1], 10, 64); err != nil {
		return 0, 0, err
	}
	return run, wait, nil
}

// nsID returns the last value of NSpid in a status file, def if there is none.
func (p Proc) nsID(status string, def int) int {
	b, err := os.ReadFile(status)
	if err != nil {
		return def
	}
	for _, l := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(l, "NSpid:"); ok {
			if fs := strings.Fields(rest); len(fs) > 0 {
				if n, err := strconv.Atoi(fs[len(fs)-1]); err == nil {
					return n
				}
			}
		}
	}
	return def
}

// statFields splits /proc/<pid>/stat; the comm field may contain spaces and parens.
func statFields(s string) []string {
	open := strings.IndexByte(s, '(')
	end := strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return nil
	}
	out := []string{strings.TrimSpace(s[:open]), s[open+1 : end]}
	return append(out, strings.Fields(s[end+1:])...)
}

func (p Proc) childMap() (map[int][]int, error) {
	entries, err := os.ReadDir(p.Root)
	if err != nil {
		return nil, err
	}
	m := map[int][]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(p.Root, e.Name(), "stat"))
		if err != nil {
			continue
		}
		f := statFields(string(b))
		if len(f) < 4 {
			continue
		}
		if ppid, err := strconv.Atoi(f[3]); err == nil {
			m[ppid] = append(m[ppid], pid)
		}
	}
	for k := range m {
		sort.Ints(m[k])
	}
	return m, nil
}

func (p Proc) isJava(pid int) bool {
	dir := filepath.Join(p.Root, strconv.Itoa(pid))
	if b, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil && strings.TrimSpace(string(b)) == "java" {
		return true
	}
	if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil && path.Base(strings.TrimSuffix(exe, " (deleted)")) == "java" {
		return true
	}
	return false
}

func (p Proc) javaProcess(pid int) (JavaProcess, error) {
	j := JavaProcess{HostPID: pid, NSPID: pid}
	dir := filepath.Join(p.Root, strconv.Itoa(pid))
	b, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return j, err
	}
	if j.StartTime, err = ctmp.ParseStartTime(b); err != nil {
		return j, err
	}
	f, err := os.Open(filepath.Join(dir, "status"))
	if err != nil {
		return j, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "NSpid:"); ok {
			if fs := strings.Fields(rest); len(fs) > 0 {
				if n, err := strconv.Atoi(fs[len(fs)-1]); err == nil {
					j.NSPID = n
				}
			}
		}
	}
	j.Tmp = ctmp.ProcTmp(dir, j.StartTime)
	// The user inside the container depends on the Wings config (and rootless
	// mode), so search hsperfdata_*/<ns-pid> instead of hard-coding it. The
	// container controls these paths: ctmp follows no symlink, also not when
	// the file is read again later.
	names, _ := j.Tmp.Names()
	for _, n := range names {
		if !strings.HasPrefix(n, "hsperfdata_") {
			continue
		}
		f := ctmp.File{Dir: j.Tmp.Sub(n), Name: strconv.Itoa(j.NSPID)}
		if m, err := f.Dir.Mode(f.Name); err == nil && m.IsRegular() {
			j.Hsperf = f
			j.User = strings.TrimPrefix(n, "hsperfdata_")
			break
		}
	}
	return j, nil
}
