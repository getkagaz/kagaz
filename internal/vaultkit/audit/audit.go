// Package audit writes the vault's append-only JSONL log. Every mutation and
// every confidential resolution lands here, one JSON object per line, so the
// log stays greppable with the tools already on the machine.
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry is one audit line.
type Entry struct {
	Time      string            `json:"time"`
	Op        string            `json:"op"`
	Actor     string            `json:"actor,omitempty"`
	Paths     []string          `json:"paths,omitempty"`
	Manifest  string            `json:"manifest,omitempty"`
	Confirmed bool              `json:"confirmed,omitempty"`
	Detail    map[string]string `json:"detail,omitempty"`
}

// Log appends entries to a vault log file.
type Log struct {
	path string
}

// Open prepares a log at path. The file is created on first write.
func Open(path string) *Log { return &Log{path: path} }

// Path is the log's location on disk.
func (l *Log) Path() string { return l.path }

// Append writes one entry. Time is stamped here if the caller left it blank.
func (l *Log) Append(e Entry) error {
	if l == nil || l.path == "" {
		return nil
	}
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339)
	}
	if e.Actor == "" {
		e.Actor = actor()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Tail returns the last n entries, oldest first. Unparseable lines are skipped
// rather than failing the read: the log is append-only and must stay readable
// even if something once wrote a bad line.
func (l *Log) Tail(n int) ([]Entry, error) {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var out []Entry
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

func actor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "unknown"
}
