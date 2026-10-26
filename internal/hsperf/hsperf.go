// Package hsperf parses HotSpot performance data files (hsperfdata, format v2).
//
// The JVM writes them to /tmp/hsperfdata_<user>/<pid>; jstat reads the same
// file. Reading never stops the JVM.
package hsperf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
)

// Magic is the first four bytes of every hsperfdata file, in big-endian order.
const Magic uint32 = 0xcafec0c0

const prologueSize = 32

// Units as stored in the entry header (PerfData::Units).
type Units uint8

const (
	UnitsNone   Units = 1
	UnitsBytes  Units = 2
	UnitsTicks  Units = 3
	UnitsEvents Units = 4
	UnitsString Units = 5
	UnitsHertz  Units = 6
)

// Errors returned by Parse.
var (
	ErrBadMagic      = errors.New("hsperf: bad magic")
	ErrBadVersion    = errors.New("hsperf: unsupported version")
	ErrTruncated     = errors.New("hsperf: truncated data")
	ErrNotAccessible = errors.New("hsperf: data not accessible yet")
	ErrNotRegular    = errors.New("hsperf: not a regular file")
)

// Value is one counter. Exactly one of Long and String is meaningful,
// depending on IsString.
type Value struct {
	Units    Units
	IsString bool
	Long     int64
	String   string
}

// Data is a parsed hsperfdata snapshot.
type Data struct {
	Major, Minor uint8
	ModTime      int64 // mod_time_stamp in ticks
	entries      map[string]Value
}

// ReadFile reads and parses an hsperfdata file. It lives in a directory the
// container controls; ctmp opens only a regular file and follows no symlink.
func ReadFile(file ctmp.File) (*Data, error) {
	f, err := file.Dir.Open(file.Name)
	if errors.Is(err, ctmp.ErrNotRegular) {
		return nil, ErrNotRegular
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The file is a fixed-size mmap (32 KB by default, PerfDataMemorySize);
	// cap reads so a hostile file cannot exhaust memory.
	b, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse parses an hsperfdata buffer.
func Parse(b []byte) (*Data, error) {
	if len(b) < prologueSize {
		return nil, ErrTruncated
	}
	if binary.BigEndian.Uint32(b[0:4]) != Magic {
		return nil, ErrBadMagic
	}
	var bo binary.ByteOrder = binary.BigEndian
	if b[4] == 1 {
		bo = binary.LittleEndian
	}
	d := &Data{Major: b[5], Minor: b[6], entries: map[string]Value{}}
	if d.Major != 2 {
		return nil, fmt.Errorf("%w: %d.%d", ErrBadVersion, d.Major, d.Minor)
	}
	if b[7] == 0 {
		return nil, ErrNotAccessible
	}
	used := int(int32(bo.Uint32(b[8:12])))
	d.ModTime = int64(bo.Uint64(b[16:24]))
	off := int(int32(bo.Uint32(b[24:28])))
	n := int(int32(bo.Uint32(b[28:32])))
	if used <= 0 || used > len(b) {
		used = len(b)
	}
	for i := 0; i < n; i++ {
		if off < prologueSize || off+20 > used {
			return nil, ErrTruncated
		}
		e := b[off:used]
		entryLen := int(int32(bo.Uint32(e[0:4])))
		nameOff := int(int32(bo.Uint32(e[4:8])))
		vecLen := int(int32(bo.Uint32(e[8:12])))
		typ := e[12]
		units := Units(e[14])
		dataOff := int(int32(bo.Uint32(e[16:20])))
		if entryLen < 20 || entryLen > len(e) || nameOff < 20 || nameOff >= entryLen || dataOff < 0 || dataOff > entryLen {
			return nil, ErrTruncated
		}
		e = e[:entryLen]
		nameEnd := bytes.IndexByte(e[nameOff:], 0)
		if nameEnd < 0 {
			return nil, ErrTruncated
		}
		name := string(e[nameOff : nameOff+nameEnd])
		v := Value{Units: units}
		switch {
		case vecLen == 0 && typ == 'J':
			if dataOff+8 > entryLen {
				return nil, ErrTruncated
			}
			v.Long = int64(bo.Uint64(e[dataOff : dataOff+8]))
		case vecLen > 0 && typ == 'B':
			if dataOff+vecLen > entryLen {
				return nil, ErrTruncated
			}
			s := e[dataOff : dataOff+vecLen]
			if z := bytes.IndexByte(s, 0); z >= 0 {
				s = s[:z]
			}
			v.IsString = true
			v.String = string(s)
		default:
			// Other types (e.g. long vectors) are not used by HotSpot; skip.
			off += entryLen
			continue
		}
		d.entries[name] = v
		off += entryLen
	}
	return d, nil
}

// Len returns the number of parsed entries.
func (d *Data) Len() int { return len(d.entries) }

// Get returns the raw entry.
func (d *Data) Get(name string) (Value, bool) {
	v, ok := d.entries[name]
	return v, ok
}

// Long returns a numeric counter.
func (d *Data) Long(name string) (int64, bool) {
	v, ok := d.entries[name]
	if !ok || v.IsString {
		return 0, false
	}
	return v.Long, true
}

// String returns a string counter.
func (d *Data) String(name string) (string, bool) {
	v, ok := d.entries[name]
	if !ok || !v.IsString {
		return "", false
	}
	return v.String, true
}

// Names returns all entry names with the given prefix, sorted.
func (d *Data) Names(prefix string) []string {
	var out []string
	for k := range d.entries {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// SetString overrides a string counter (for tests of callers).
func (d *Data) SetString(name, v string) {
	d.entries[name] = Value{Units: UnitsString, IsString: true, String: v}
}

// SetLong overrides a numeric counter (for tests of callers).
func (d *Data) SetLong(name string, v int64) {
	d.entries[name] = Value{Units: UnitsNone, Long: v}
}
