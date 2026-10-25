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
)

// ErrNoJava means no java process runs in the container.
var ErrNoJava = errors.New("container: no java process")

// JavaProcess is the JVM found in a container.
type JavaProcess struct {
	HostPID int
	NSPID   int    // PID inside the container (last value of NSpid)
	User    string // from /tmp/hsperfdata_<user>; empty if no hsperfdata file
	Hsperf  string // host path of the hsperfdata file, empty if none
	TmpDir  string // host path of the container's /tmp
}

// AttachSocket is the host path of the attach socket (/tmp/.java_pid<ns-pid>).
func (j JavaProcess) AttachSocket() string {
	return filepath.Join(j.TmpDir, ".java_pid"+strconv.Itoa(j.NSPID))
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
				if j.Hsperf != "" {
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
	f, err := os.Open(filepath.Join(p.Root, strconv.Itoa(pid), "status"))
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
	j.TmpDir = filepath.Join(p.Root, strconv.Itoa(pid), "root", "tmp")
	// The user inside the container depends on the Wings config (and rootless
	// mode), so search hsperfdata_*/<ns-pid> instead of hard-coding it. The
	// container controls these paths: never follow symlinks (an absolute link
	// would resolve against the host root).
	entries, _ := os.ReadDir(j.TmpDir)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "hsperfdata_") {
			continue
		}
		m := filepath.Join(j.TmpDir, e.Name(), strconv.Itoa(j.NSPID))
		if st, err := os.Lstat(m); err == nil && st.Mode().IsRegular() {
			j.Hsperf = m
			j.User = strings.TrimPrefix(e.Name(), "hsperfdata_")
			break
		}
	}
	return j, nil
}
