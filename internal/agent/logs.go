package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Line is one captured output line. The on-disk format is framed rather than raw
// because shipping anywhere needs the timestamp and stream that raw bytes discard,
// and migrating the format later would be worse than paying for it now.
type Line struct {
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
}

// Ring is a size-bounded sequence of segment files.
//
// It is deliberately small. Bytes written in a sandbox cost memory and not just
// disk: they land in page cache, and a pause snapshots guest RAM, so a large cache
// inflates every snapshot and slows every sleep. The agent therefore keeps only a
// live window and drops the pages it has written.
type Ring struct {
	dir        string
	name       string
	maxBytes   int64
	segBytes   int64
	mu         sync.Mutex
	cur        *os.File
	curBytes   int64
	seq        int64
	offset     int64
	droppedSeq int64
}

const defaultSegments = 4

func OpenRing(dir, name string, maxBytes int64) (*Ring, error) {
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r := &Ring{dir: dir, name: name, maxBytes: maxBytes, segBytes: maxBytes / defaultSegments}
	if r.segBytes < 64<<10 {
		r.segBytes = 64 << 10
	}
	segs, err := r.segments()
	if err != nil {
		return nil, err
	}
	if len(segs) > 0 {
		last := segs[len(segs)-1]
		r.seq = last.seq
		if st, err := os.Stat(last.path); err == nil {
			r.curBytes = st.Size()
		}
		f, err := os.OpenFile(last.path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		r.cur = f
		r.droppedSeq = segs[0].seq
	} else if err := r.rotate(); err != nil {
		return nil, err
	}
	return r, nil
}

type segment struct {
	seq  int64
	path string
}

func (r *Ring) segments() ([]segment, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, err
	}
	var out []segment
	prefix := r.name + "."
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		mid := strings.TrimSuffix(strings.TrimPrefix(e.Name(), prefix), ".jsonl")
		n, err := strconv.ParseInt(mid, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, segment{seq: n, path: filepath.Join(r.dir, e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out, nil
}

func (r *Ring) rotate() error {
	if r.cur != nil {
		r.cur.Sync()
		r.cur.Close()
	}
	r.seq++
	path := filepath.Join(r.dir, fmt.Sprintf("%s.%d.jsonl", r.name, r.seq))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	r.cur = f
	r.curBytes = 0
	return r.trim()
}

// trim keeps total size under the cap. The cap always wins: an unshipped sink
// delays rotation only inside it, because suspending rotation is exactly how a
// broken sink fills a disk.
func (r *Ring) trim() error {
	segs, err := r.segments()
	if err != nil {
		return err
	}
	var total int64
	sizes := make([]int64, len(segs))
	for i, s := range segs {
		if st, err := os.Stat(s.path); err == nil {
			sizes[i] = st.Size()
			total += st.Size()
		}
	}
	for i := 0; i < len(segs)-1 && total > r.maxBytes; i++ {
		if err := os.Remove(segs[i].path); err != nil {
			return err
		}
		total -= sizes[i]
		r.droppedSeq = segs[i].seq
	}
	return nil
}

// Write appends one line.
func (r *Ring) Write(stream, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, err := json.Marshal(Line{TS: time.Now().UTC(), Stream: stream, Text: text})
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if r.curBytes+int64(len(raw)) > r.segBytes {
		if err := r.rotate(); err != nil {
			return err
		}
	}
	n, err := r.cur.Write(raw)
	r.curBytes += int64(n)
	r.offset += int64(n)
	return err
}

// Flush commits to the device and asks the kernel to drop the pages written, so
// the log does not sit in page cache and inflate the next pause snapshot.
func (r *Ring) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return
	}
	r.cur.Sync()
	dropPageCache(r.cur)
}

func (r *Ring) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return nil
	}
	r.cur.Sync()
	err := r.cur.Close()
	r.cur = nil
	return err
}

// Read returns lines from the ring, and reports whether anything was dropped
// before them so a gap is visible rather than silent.
func (r *Ring) Read(fromSeq int64, limit int) (lines []Line, dropped bool, lastSeq int64, err error) {
	segs, err := r.segments()
	if err != nil {
		return nil, false, 0, err
	}
	if len(segs) > 0 && fromSeq > 0 && fromSeq < segs[0].seq {
		dropped = true
	}
	for _, s := range segs {
		if s.seq < fromSeq {
			continue
		}
		raw, err := os.ReadFile(s.path)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if l == "" {
				continue
			}
			var ln Line
			if json.Unmarshal([]byte(l), &ln) == nil {
				lines = append(lines, ln)
			}
		}
		lastSeq = s.seq
	}
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, dropped, lastSeq, nil
}

// Dropped reports the highest segment discarded to stay under the cap.
func (r *Ring) Dropped() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.droppedSeq
}
