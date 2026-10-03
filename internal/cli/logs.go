package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danthegoodman1/simplecloud/internal/agent"
	"github.com/danthegoodman1/simplecloud/internal/deploy"
	"github.com/danthegoodman1/simplecloud/internal/state"
)

// historyPath is where a slot's pulled log history accumulates on this machine.
//
// The sandbox keeps only a small live window, because bytes written there land in
// page cache and a pause snapshots RAM. History belongs here, where disk is cheap
// and nothing touches a snapshot.
func (e *env) historyPath(proj *state.Project, slot string) string {
	return filepath.Join(e.cfg.LogsPath(), proj.ID, slot+".jsonl")
}

// pullLogs appends whatever the agent has that this machine does not.
//
// It rides the activity poll's round trip rather than costing another, which is
// what makes a 4 MiB ring in the sandbox safe: the cron cadence that drives reap
// becomes the log capture cadence.
func (e *env) pullLogs(d *deploy.Deployer, s *state.Slot) {
	chunk, err := d.AgentLogs(s, s.LogCursor, 0, "")
	if err != nil {
		return
	}
	proj, err := e.store.ProjectForDir(".")
	if err != nil {
		if p, err2 := e.store.ListProjects(); err2 == nil {
			for _, candidate := range p {
				if candidate.ID == s.ProjectID {
					proj = candidate
					break
				}
			}
		}
	}
	if proj == nil {
		return
	}
	if len(chunk.Lines) == 0 && !chunk.Dropped {
		if chunk.Segment > s.LogCursor {
			s.LogCursor = chunk.Segment
			_ = e.store.PutSlot(s)
		}
		return
	}
	path := e.historyPath(proj, s.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if chunk.Dropped {
		// A gap is recorded rather than hidden: the ring wrapped between pulls.
		note, _ := json.Marshal(agent.Line{TS: time.Now().UTC(), Stream: "simplecloud",
			Text: "--- some output was dropped before this point: the sandbox ring wrapped between pulls ---"})
		f.Write(append(note, '\n'))
	}
	enc := json.NewEncoder(f)
	for _, l := range chunk.Lines {
		_ = enc.Encode(l)
	}
	s.LogCursor = chunk.Segment
	_ = e.store.PutSlot(s)
	e.pruneHistory()
}

// historyCap bounds the local store. Local disk is cheap, but not unbounded.
const historyCap = 512 << 20

func (e *env) pruneHistory() {
	root := e.cfg.LogsPath()
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		files = append(files, file{path, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if total <= historyCap {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= historyCap {
			return
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}

// readHistory returns the locally stored lines for a slot.
func (e *env) readHistory(proj *state.Project, slot string, tail int) []agent.Line {
	raw, err := os.ReadFile(e.historyPath(proj, slot))
	if err != nil {
		return nil
	}
	var out []agent.Line
	for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if l == "" {
			continue
		}
		var line agent.Line
		if json.Unmarshal([]byte(l), &line) == nil {
			out = append(out, line)
		}
	}
	if tail > 0 && len(out) > tail {
		out = out[len(out)-tail:]
	}
	return out
}

func formatLine(slot string, l agent.Line, showTS, prefix bool) string {
	var b strings.Builder
	if prefix {
		fmt.Fprintf(&b, "%-14s| ", slot)
	}
	if showTS {
		b.WriteString(l.TS.Format(time.RFC3339) + " ")
	}
	b.WriteString(l.Text)
	return b.String()
}
