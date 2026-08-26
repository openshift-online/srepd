package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeIndexLines writes raw lines to a session dir's index.jsonl.
func writeIndexLines(t *testing.T, dir string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, "index.jsonl")
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return path
}

func entryLine(t *testing.T, incidentID string, sid uuid.UUID) string {
	t.Helper()
	data, err := json.Marshal(sessionEntry{
		IncidentID: incidentID,
		SessionID:  sid.String(),
		Created:    time.Now(),
	})
	require.NoError(t, err)
	return string(data)
}

// assertNoTempFiles fails if compaction left a ".index-*.jsonl" temp file
// behind in dir. Every compact() failure path must remove its temp file.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".index-"),
			"compaction leaked a temp file: %s", e.Name())
	}
}

// requireNonRoot skips a test that relies on filesystem permission bits.
// root bypasses mode checks entirely, so the failure these tests provoke
// would silently not happen and the test would pass for the wrong reason.
func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("test relies on filesystem permission bits; root bypasses them")
	}
}

// oversizedIndex writes an index.jsonl with enough lines to push load() past
// indexCompactionThreshold, guaranteeing compact() is called. It returns the
// index path and the file's exact contents before compaction.
func oversizedIndex(t *testing.T, dir string) (string, []byte) {
	t.Helper()
	var lines []string
	for i := 0; i < indexCompactionThreshold+50; i++ {
		// Few distinct incidents so compaction would visibly shrink the file.
		lines = append(lines, entryLine(t, fmt.Sprintf("INC-%03d", i%25), uuid.New()))
	}
	path := writeIndexLines(t, dir, lines)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	return path, before
}

func readIndexLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// --- SessionManager.evicted bounding ---

func newTestManager(t *testing.T, maxLive int) *SessionManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &SessionManager{
		sessions: make(map[string]*Session),
		evicted:  make(map[string]bool),
		maxLive:  maxLive,
		cfg:      Config{CLICommand: "claude", SessionEnabled: true, MaxSessions: maxLive},
		exec: &delegatingExecutor{
			create: func() *mockStreamExecutor {
				return newMockStreamExecutor(`{"type":"system","subtype":"init","session_id":"test"}` + "\n")
			},
			sessions: make(map[string]*mockStreamExecutor),
		},
		index:     newSessionIndex(""),
		ctx:       ctx,
		ctxCancel: cancel,
	}
}

// TestSessionManager_EvictedClearedOnResume covers the unbounded-map defect:
// evicted recorded every incident ever evicted and never removed anything, so
// it grew for the life of the process. Once an incident is resumed the flag
// has done its job and must be dropped.
func TestSessionManager_EvictedClearedOnResume(t *testing.T) {
	mgr := newTestManager(t, 1)

	mgr.GetOrCreate("INC-001", nil)
	mgr.GetOrCreate("INC-002", nil) // evicts INC-001

	mgr.mu.Lock()
	assert.True(t, mgr.evicted["INC-001"], "INC-001 must be marked evicted")
	mgr.mu.Unlock()

	s := mgr.GetOrCreate("INC-001", nil) // resumes INC-001, evicts INC-002
	assert.True(t, s.resumed, "the resumed session must still use --resume")

	mgr.mu.Lock()
	_, stillThere := mgr.evicted["INC-001"]
	mgr.mu.Unlock()
	assert.False(t, stillThere,
		"the evicted flag must be dropped once it has been consumed")
}

// TestSessionManager_EvictedStaysBounded drives many distinct incidents
// through a small manager and asserts the map does not grow without limit.
func TestSessionManager_EvictedStaysBounded(t *testing.T) {
	mgr := newTestManager(t, 2)

	const incidents = 500
	for i := 0; i < incidents; i++ {
		mgr.GetOrCreate(fmt.Sprintf("INC-%04d", i), nil)
	}

	mgr.mu.Lock()
	size := len(mgr.evicted)
	mgr.mu.Unlock()

	assert.LessOrEqual(t, size, maxEvictedEntries,
		"the evicted map must stay bounded (had %d entries after %d incidents)", size, incidents)
}

// TestSessionManager_ResumeStillWorksAfterBounding ensures bounding does not
// break the resume contract for a recently evicted incident.
func TestSessionManager_ResumeStillWorksAfterBounding(t *testing.T) {
	mgr := newTestManager(t, 2)

	mgr.GetOrCreate("INC-A", nil)
	mgr.GetOrCreate("INC-B", nil)
	mgr.GetOrCreate("INC-C", nil) // evicts INC-A

	s := mgr.GetOrCreate("INC-A", nil)
	assert.True(t, s.resumed, "a recently evicted incident must resume")
}

// --- index.jsonl load-time compaction ---

func TestSessionIndex_CompactsOversizedFile(t *testing.T) {
	dir := t.TempDir()

	// One incident re-recorded many times: only the last entry per incident
	// is meaningful, so compaction should collapse this dramatically.
	var lines []string
	var lastSID uuid.UUID
	for i := 0; i < indexCompactionThreshold+50; i++ {
		lastSID = uuid.New()
		lines = append(lines, entryLine(t, "INC-HOT", lastSID))
	}
	path := writeIndexLines(t, dir, lines)

	idx := newSessionIndex(dir)

	assert.True(t, idx.has("INC-HOT"), "the retained entry must survive compaction")
	idx.mu.Lock()
	got := idx.established["INC-HOT"]
	idx.mu.Unlock()
	assert.Equal(t, lastSID, got, "compaction must keep the LAST entry per incident")

	after := readIndexLines(t, path)
	assert.Len(t, after, 1, "compaction must collapse to one line per incident")
}

func TestSessionIndex_CompactionKeepsEveryIncident(t *testing.T) {
	dir := t.TempDir()

	want := make(map[string]uuid.UUID)
	var lines []string

	// 200 distinct incidents, each recorded twice; plus enough churn on one
	// incident to cross the threshold.
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("INC-%03d", i)
		_ = entryLine(t, id, uuid.New())
		lines = append(lines, entryLine(t, id, uuid.New()))
		sid := uuid.New()
		lines = append(lines, entryLine(t, id, sid))
		want[id] = sid
	}
	for i := 0; i < indexCompactionThreshold; i++ {
		sid := uuid.New()
		lines = append(lines, entryLine(t, "INC-CHURN", sid))
		want["INC-CHURN"] = sid
	}
	path := writeIndexLines(t, dir, lines)

	idx := newSessionIndex(dir)

	for id, sid := range want {
		idx.mu.Lock()
		got, ok := idx.established[id]
		idx.mu.Unlock()
		require.True(t, ok, "incident %s lost during compaction", id)
		assert.Equal(t, sid, got, "incident %s kept the wrong session id", id)
	}

	after := readIndexLines(t, path)
	assert.Len(t, after, len(want),
		"the compacted file must hold exactly one line per retained incident")

	// Reloading the compacted file must produce the same mapping — no data loss.
	reloaded := newSessionIndex(dir)
	for id, sid := range want {
		reloaded.mu.Lock()
		got, ok := reloaded.established[id]
		reloaded.mu.Unlock()
		require.True(t, ok, "incident %s lost on reload", id)
		assert.Equal(t, sid, got)
	}
}

func TestSessionIndex_BelowThresholdIsNotRewritten(t *testing.T) {
	dir := t.TempDir()

	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, entryLine(t, fmt.Sprintf("INC-%02d", i), uuid.New()))
	}
	path := writeIndexLines(t, dir, lines)

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	newSessionIndex(dir)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"a file under the threshold must be left byte-identical")
}

func TestSessionIndex_ExactlyAtThresholdIsNotRewritten(t *testing.T) {
	dir := t.TempDir()

	var lines []string
	for i := 0; i < indexCompactionThreshold; i++ {
		lines = append(lines, entryLine(t, fmt.Sprintf("INC-%04d", i), uuid.New()))
	}
	path := writeIndexLines(t, dir, lines)

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	newSessionIndex(dir)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"compaction triggers above the threshold, not at it")
}

func TestSessionIndex_ToleratesCorruptLinesDuringCompaction(t *testing.T) {
	dir := t.TempDir()

	goodSID := uuid.New()
	var lines []string
	for i := 0; i < indexCompactionThreshold; i++ {
		lines = append(lines, entryLine(t, fmt.Sprintf("INC-%04d", i), uuid.New()))
	}
	lines = append(lines, "{not json at all")
	lines = append(lines, `{"incident_id":"INC-BADUUID","session_id":"not-a-uuid","created":"2026-01-01T00:00:00Z"}`)
	lines = append(lines, entryLine(t, "INC-GOOD", goodSID))
	path := writeIndexLines(t, dir, lines)

	idx := newSessionIndex(dir)

	assert.True(t, idx.has("INC-GOOD"), "valid entries after corrupt lines must survive")
	idx.mu.Lock()
	got := idx.established["INC-GOOD"]
	_, badPresent := idx.established["INC-BADUUID"]
	idx.mu.Unlock()
	assert.Equal(t, goodSID, got)
	assert.False(t, badPresent, "an unparseable session id must not be retained")

	// The rewritten file must contain only well-formed entries.
	for _, line := range readIndexLines(t, path) {
		var e sessionEntry
		require.NoError(t, json.Unmarshal([]byte(line), &e),
			"compaction must not write malformed lines")
		_, err := uuid.Parse(e.SessionID)
		require.NoError(t, err)
	}
}

func TestSessionIndex_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeIndexLines(t, dir, nil)

	assert.NotPanics(t, func() { newSessionIndex(dir) })

	after := readIndexLines(t, path)
	assert.Empty(t, after, "an empty file stays empty")
}

func TestSessionIndex_MissingFile(t *testing.T) {
	dir := t.TempDir()
	assert.NotPanics(t, func() { newSessionIndex(dir) })
	_, err := os.Stat(filepath.Join(dir, "index.jsonl"))
	assert.True(t, os.IsNotExist(err), "compaction must not create a file that was absent")
}

// TestSessionIndex_CompactionIsAtomic asserts the rewrite goes through a temp
// file and a rename rather than truncating the live file in place — a crash
// mid-rewrite must never leave a half-written index.
func TestSessionIndex_CompactionIsAtomic(t *testing.T) {
	dir := t.TempDir()

	var lines []string
	for i := 0; i < indexCompactionThreshold+10; i++ {
		lines = append(lines, entryLine(t, fmt.Sprintf("INC-%04d", i%50), uuid.New()))
	}
	path := writeIndexLines(t, dir, lines)

	newSessionIndex(dir)

	// The rewrite must leave no temp files behind.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.Equal(t, "index.jsonl", e.Name(),
			"compaction must not leave stray files in the session dir")
	}

	// And the file must be complete and parseable.
	for _, line := range readIndexLines(t, path) {
		var e sessionEntry
		require.NoError(t, json.Unmarshal([]byte(line), &e))
	}
	assert.Equal(t, 50, len(readIndexLines(t, path)))
}

// TestSessionIndex_RecordAfterCompactionAppends verifies the index is still
// writable after a compaction pass.
func TestSessionIndex_RecordAfterCompactionAppends(t *testing.T) {
	dir := t.TempDir()

	var lines []string
	for i := 0; i < indexCompactionThreshold+5; i++ {
		lines = append(lines, entryLine(t, fmt.Sprintf("INC-%04d", i%20), uuid.New()))
	}
	path := writeIndexLines(t, dir, lines)

	idx := newSessionIndex(dir)
	compacted := len(readIndexLines(t, path))

	newSID := uuid.New()
	idx.record("INC-BRANDNEW", newSID)

	after := readIndexLines(t, path)
	assert.Len(t, after, compacted+1, "record must append after compaction")

	reloaded := newSessionIndex(dir)
	assert.True(t, reloaded.has("INC-BRANDNEW"),
		"the appended entry must survive a reload")
}

// --- compact() failure paths ---
//
// compact() rewrites a file on disk. Every one of its error paths must hold
// the same two guarantees: the ORIGINAL index.jsonl is left byte-identical,
// and no temp file is leaked. The tests below drive the real syscalls into
// failure rather than stubbing them, so they exercise the production code.

// TestCompact_RenameFailure covers index.go:178-181 — the atomic replace
// itself failing. This is the highest-value case: the rename is the single
// instant where the live index is swapped, and a mishandled failure here is
// what would corrupt or destroy the user's session index.
//
// The rename is forced to fail by making the target path a NON-EMPTY
// directory: rename(2) refuses to replace a non-empty directory with a file
// (ENOTEMPTY/EEXIST/EISDIR depending on kernel), so os.Rename returns an
// error while the temp file it was moving still exists.
func TestCompact_RenameFailure(t *testing.T) {
	dir := t.TempDir()

	// A real, populated index.jsonl living alongside the compaction target.
	// It is the bystander: nothing about a failed compaction may disturb it.
	var lines []string
	for i := 0; i < indexCompactionThreshold+50; i++ {
		lines = append(lines, entryLine(t, fmt.Sprintf("INC-%03d", i%25), uuid.New()))
	}
	path := writeIndexLines(t, dir, lines)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	// compact() is driven directly here rather than through load(). The
	// rename is the LAST step, so reaching it requires CreateTemp, the
	// writes, Sync and Close to all succeed first — meaning the failure has
	// to be planted at the destination, not in the directory or the fd.
	// A non-empty directory at idx.path is exactly that: rename(2) will not
	// replace it with a file, and the temp file survives the failed call.
	target := filepath.Join(dir, "locked")
	require.NoError(t, os.Mkdir(target, 0700))
	require.NoError(t,
		os.WriteFile(filepath.Join(target, "occupant"), []byte("x"), 0600))

	idx := &sessionIndex{
		established: make(map[string]uuid.UUID),
		path:        target, // renaming a file over a non-empty dir must fail
	}
	order := []string{"INC-A", "INC-B"}
	loaded := map[string]loadedEntry{
		"INC-A": {sessionID: uuid.New(), created: time.Now()},
		"INC-B": {sessionID: uuid.New(), created: time.Now()},
	}

	idx.compact(order, loaded)

	// The destination is untouched: still a directory, still holding its file.
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "a failed rename must not replace the target")
	occupant, err := os.ReadFile(filepath.Join(target, "occupant"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(occupant),
		"the destination's contents must survive a failed rename")

	// The unrelated real index is byte-identical.
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"a failed compaction must leave the original index byte-identical")

	// And the temp file was cleaned up by fail().
	assertNoTempFiles(t, dir)
}

// TestCompact_TempCreateFailure covers index.go:125-127 — os.CreateTemp
// failing. The session directory is made read-only (0500), so a new file
// cannot be created in it, while the existing index.jsonl remains readable.
//
// The contract under test is degraded-but-correct: compaction is skipped,
// the on-disk file is untouched, and the in-memory map is still FULLY
// populated from the successful read. A failure to compact must never cost
// the caller its session lookups.
func TestCompact_TempCreateFailure(t *testing.T) {
	requireNonRoot(t)

	dir := t.TempDir()
	path, before := oversizedIndex(t, dir)

	// Read-only directory: open(O_CREAT) inside it fails with EACCES.
	require.NoError(t, os.Chmod(dir, 0500))
	// Restore so t.TempDir()'s cleanup can remove the directory.
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	idx := newSessionIndex(dir)

	// The original file is byte-identical — compaction never got started.
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"a CreateTemp failure must leave index.jsonl untouched")

	// Degraded but correct: every incident read from disk is still in memory.
	idx.mu.Lock()
	loaded := len(idx.established)
	idx.mu.Unlock()
	assert.Equal(t, 25, loaded,
		"a failed compaction must not cost the in-memory index its entries")
	for i := 0; i < 25; i++ {
		assert.True(t, idx.has(fmt.Sprintf("INC-%03d", i)),
			"incident INC-%03d must still resolve after a failed compaction", i)
	}

	assertNoTempFiles(t, dir)
}

// Unforceable paths — deliberately NOT tested.
//
// Four of compact()'s error branches cannot be provoked from a test without
// adding a seam to production code, so no test here pretends to cover them:
//
//	index.go:155-158  tmp.Sync() failure
//	index.go:159-162  tmp.Write() failure
//	index.go:166-169  tmp.Close() failure
//	index.go:174-177  os.Chmod(tmpName) failure
//
// All four act on the fd returned by os.CreateTemp, which is a local
// variable inside compact(). A test cannot reach it. The usual tricks were
// tried and rejected:
//
//   - Unlinking the temp file mid-compaction does not break the fd; writes
//     and Sync continue to succeed against the now-anonymous inode.
//   - A read-only parent directory makes CreateTemp fail first (covered by
//     TestCompact_TempCreateFailure), so execution never reaches the writes.
//   - RLIMIT_FSIZE does force EFBIG on write, but the limit applies to every
//     fd in the process — including the test binary's own stdout — which
//     corrupts the harness's output. Not usable in-process.
//   - Filling the filesystem to force ENOSPC requires a dedicated small
//     mount, i.e. root.
//
// Covering them honestly would mean injecting a filesystem interface into
// sessionIndex. That is a production change to serve a test, on a routine
// whose real risk is concentrated in the rename — which IS covered above.
// The fail() cleanup closure is exercised through the rename path, so the
// temp-file-removal behaviour it implements is tested; only the four
// specific triggers above are not.

// TestCompact_SuccessPathControl is the control for the failure tests: with
// nothing obstructing it, the same fixture those tests use must compact
// cleanly. Without this, a failure test could pass because compaction never
// ran at all rather than because it failed and recovered.
func TestCompact_SuccessPathControl(t *testing.T) {
	dir := t.TempDir()
	path, before := oversizedIndex(t, dir)

	idx := newSessionIndex(dir)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotEqual(t, string(before), string(after),
		"an unobstructed compaction rewrites the file")
	assert.Len(t, readIndexLines(t, path), 25,
		"compaction collapses to one line per incident")

	idx.mu.Lock()
	loaded := len(idx.established)
	idx.mu.Unlock()
	assert.Equal(t, 25, loaded)

	// The rewritten file must be mode 0600 — compaction must not widen
	// permissions on a file that records session identifiers.
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(),
		"the compacted index must keep 0600")

	assertNoTempFiles(t, dir)
}

// TestCompact_FailurePathsLeaveNoTempFile is the cross-cutting invariant:
// whatever goes wrong, compact() must not leak a ".index-*.jsonl" file into
// the user's session directory. Leaked temp files would accumulate silently,
// one per failed startup.
func TestCompact_FailurePathsLeaveNoTempFile(t *testing.T) {
	requireNonRoot(t)

	t.Run("rename failure", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "index.jsonl")
		require.NoError(t, os.Mkdir(target, 0700))
		require.NoError(t,
			os.WriteFile(filepath.Join(target, "occupant"), []byte("x"), 0600))

		idx := &sessionIndex{
			established: make(map[string]uuid.UUID),
			path:        target,
		}
		idx.compact([]string{"INC-A"}, map[string]loadedEntry{
			"INC-A": {sessionID: uuid.New(), created: time.Now()},
		})

		assertNoTempFiles(t, dir)
	})

	t.Run("empty order is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		idx := &sessionIndex{
			established: make(map[string]uuid.UUID),
			path:        filepath.Join(dir, "index.jsonl"),
		}
		idx.compact(nil, nil)

		assertNoTempFiles(t, dir)
		_, err := os.Stat(filepath.Join(dir, "index.jsonl"))
		assert.True(t, os.IsNotExist(err),
			"an empty compaction must not create a file")
	})

	t.Run("empty path is a no-op", func(t *testing.T) {
		idx := &sessionIndex{established: make(map[string]uuid.UUID)}
		assert.NotPanics(t, func() {
			idx.compact([]string{"INC-A"}, map[string]loadedEntry{
				"INC-A": {sessionID: uuid.New(), created: time.Now()},
			})
		})
	})
}

// TestCompact_MissingLoadedEntryIsSkipped covers index.go:147-148 — the
// guard for an incident present in `order` but absent from `loaded`. The two
// are built together in load(), so this cannot happen today; the branch is a
// defensive one and is pinned here so a future caller that passes mismatched
// arguments produces a short file rather than a panic or a zero-value entry.
func TestCompact_MissingLoadedEntryIsSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")

	keptSID := uuid.New()
	idx := &sessionIndex{
		established: make(map[string]uuid.UUID),
		path:        path,
	}

	// "INC-GHOST" is in order but has no loaded entry: it must be skipped,
	// not written as a zero-value line.
	idx.compact(
		[]string{"INC-GHOST", "INC-REAL"},
		map[string]loadedEntry{
			"INC-REAL": {sessionID: keptSID, created: time.Now()},
		},
	)

	lines := readIndexLines(t, path)
	require.Len(t, lines, 1, "the entry with no loaded data must be skipped")

	var e sessionEntry
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &e))
	assert.Equal(t, "INC-REAL", e.IncidentID)
	assert.Equal(t, keptSID.String(), e.SessionID)

	assertNoTempFiles(t, dir)
}
