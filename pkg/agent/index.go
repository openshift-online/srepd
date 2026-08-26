package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	charlog "github.com/charmbracelet/log"
	"github.com/google/uuid"
)

// indexCompactionThreshold is the line count above which load() rewrites
// index.jsonl, keeping only the last entry per incident. The file is
// append-only during normal operation, so an incident revisited many times
// accumulates a line each session; without compaction the file grows without
// bound and load() slows down proportionally.
const indexCompactionThreshold = 1000

type sessionEntry struct {
	IncidentID string    `json:"incident_id"`
	SessionID  string    `json:"session_id"`
	Created    time.Time `json:"created"`
}

// loadedEntry is an entry as read from disk, retaining the original Created
// timestamp so a compacting rewrite does not restamp history.
type loadedEntry struct {
	sessionID uuid.UUID
	created   time.Time
}

type sessionIndex struct {
	mu          sync.Mutex
	established map[string]uuid.UUID // incidentID -> sessionID
	path        string               // path to index.jsonl
	warned      bool                 // suppress repeated I/O warnings
}

func newSessionIndex(sessionDir string) *sessionIndex {
	idx := &sessionIndex{
		established: make(map[string]uuid.UUID),
	}
	if sessionDir == "" {
		return idx
	}
	idx.path = filepath.Join(sessionDir, "index.jsonl")
	idx.load()
	return idx
}

func (idx *sessionIndex) load() {
	if idx.path == "" {
		return
	}

	f, err := os.Open(idx.path)
	if err != nil {
		if !os.IsNotExist(err) {
			charlog.Warn("agent.index.load", "error", err)
		}
		return
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	var lastLine []byte
	lineNum := 0

	// order preserves first-seen incident order so a compacted rewrite is
	// deterministic rather than following Go's randomized map iteration.
	// loaded retains each entry's original Created stamp for the rewrite.
	var order []string
	loaded := make(map[string]loadedEntry)

	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		var entry sessionEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			lastLine = append([]byte{}, line...)
			continue
		}
		lastLine = nil
		sid, parseErr := uuid.Parse(entry.SessionID)
		if parseErr != nil {
			charlog.Warn("agent.index.load", "line", lineNum, "error", parseErr)
			continue
		}
		if _, seen := idx.established[entry.IncidentID]; !seen {
			order = append(order, entry.IncidentID)
		}
		// Last entry per incident wins: a later line supersedes an earlier one.
		idx.established[entry.IncidentID] = sid
		loaded[entry.IncidentID] = loadedEntry{sessionID: sid, created: entry.Created}
	}

	if err := scanner.Err(); err != nil {
		charlog.Warn("agent.index.load", "msg", "scanner error", "error", err)
		// A partially-read file would compact away entries we never saw.
		return
	}

	if lastLine != nil {
		charlog.Warn("agent.index.load",
			"msg", "corrupt trailing line ignored",
			"line", lineNum,
			"len", len(lastLine))
	}

	if lineNum > indexCompactionThreshold {
		idx.compact(order, loaded)
	}
}

// compact rewrites index.jsonl with one line per incident, in the given order.
// It is called from load() before the index is shared, so it needs no locking.
//
// The rewrite is atomic: a temp file in the same directory is written, synced,
// and renamed over the original. The file is never truncated in place, so a
// crash mid-rewrite leaves the previous index intact.
func (idx *sessionIndex) compact(order []string, loaded map[string]loadedEntry) {
	if idx.path == "" || len(order) == 0 {
		return
	}

	dir := filepath.Dir(idx.path)
	tmp, err := os.CreateTemp(dir, ".index-*.jsonl")
	if err != nil {
		charlog.Warn("agent.index.compact", "msg", "cannot create temp file", "error", err)
		return
	}
	tmpName := tmp.Name()

	// Any failure past this point must leave the original file untouched.
	fail := func(msg string, err error) {
		charlog.Warn("agent.index.compact", "msg", msg, "error", err)
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	written := 0
	for _, incidentID := range order {
		entry, ok := loaded[incidentID]
		if !ok {
			continue
		}
		data, err := json.Marshal(sessionEntry{
			IncidentID: incidentID,
			SessionID:  entry.sessionID.String(),
			Created:    entry.created,
		})
		if err != nil {
			fail("cannot marshal entry", err)
			return
		}
		if _, err := tmp.Write(append(data, '\n')); err != nil {
			fail("cannot write temp file", err)
			return
		}
		written++
	}

	if err := tmp.Sync(); err != nil {
		fail("cannot sync temp file", err)
		return
	}
	if err := tmp.Close(); err != nil {
		fail("cannot close temp file", err)
		return
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		fail("cannot chmod temp file", err)
		return
	}
	if err := os.Rename(tmpName, idx.path); err != nil {
		fail("cannot rename temp file over index", err)
		return
	}

	charlog.Info("agent.index.compact", "msg", "index compacted", "entries", written)
}

func (idx *sessionIndex) has(incidentID string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	_, ok := idx.established[incidentID]
	return ok
}

func (idx *sessionIndex) record(incidentID string, sessionID uuid.UUID) {
	idx.mu.Lock()
	if _, exists := idx.established[incidentID]; exists {
		idx.mu.Unlock()
		return
	}
	idx.established[incidentID] = sessionID
	path := idx.path
	warned := idx.warned
	idx.mu.Unlock()

	if path == "" {
		return
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		idx.mu.Lock()
		if !idx.warned {
			charlog.Warn("agent.index.record", "msg", "cannot create session dir", "error", err)
			idx.warned = true
		}
		idx.mu.Unlock()
		return
	}

	entry := sessionEntry{
		IncidentID: incidentID,
		SessionID:  sessionID.String(),
		Created:    time.Now(),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		if !warned {
			idx.mu.Lock()
			if !idx.warned {
				charlog.Warn("agent.index.record", "msg", "cannot write index", "error", err)
				idx.warned = true
			}
			idx.mu.Unlock()
		}
		return
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(append(data, '\n')); err != nil {
		charlog.Warn("agent.index.record", "msg", "write failed", "error", err)
	}
}
