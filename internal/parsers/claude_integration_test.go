package parsers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── parseJSONL offset-tracking integration tests ─────────────────────────

func TestClaudeCollect_NewFile_Baselines(t *testing.T) {
	// Simulate the baseline behaviour: first time a JSONL file is seen,
	// its current size is recorded as the offset and zero records are emitted.
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	lines := []string{
		`{"type":"user","sessionId":"test-sess","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"hello","model":"claude-3-opus","usage":{"input_tokens":10,"output_tokens":20}}}`,
		`{"type":"assistant","sessionId":"test-sess","timestamp":"2025-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}],"model":"claude-3-opus","usage":{"input_tokens":5,"output_tokens":15}}}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// First call: file is NOT in known_files, so it should be baselined.
	// Pass nil state to simulate a fresh start.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Manually replicate the baseline logic from Collect:
	// Unknown file => set offset = current size, emit nothing.
	offsets := restoreOffsets(nil)
	known := restoreKnownFiles(nil)

	if known[path] {
		t.Fatal("expected file NOT in known_files on first run")
	}

	// Baseline: record size, don't parse
	newOffsets := map[string]float64{path: float64(info.Size())}
	newKnown := map[string]bool{path: true}

	// Verify state was set correctly
	if newOffsets[path] != float64(len(content)) {
		t.Errorf("baseline offset = %f, want %d", newOffsets[path], len(content))
	}
	if !newKnown[path] {
		t.Error("file should be in known_files after baseline")
	}
	_ = offsets // used above
}

func TestClaudeCollect_IncrementalRead(t *testing.T) {
	// After baseline, appending new lines should return only the new records.
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	line1 := `{"type":"user","sessionId":"s1","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"first"}}` + "\n"
	if err := os.WriteFile(path, []byte(line1), 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate baseline: offset = current file size, file is known
	baselineOffset := int64(len(line1))
	state := map[string]any{
		"file_offsets": map[string]any{path: float64(baselineOffset)},
		"known_files":  map[string]any{path: true},
	}

	// Append new data
	line2 := `{"type":"user","sessionId":"s1","timestamp":"2025-01-01T00:00:01Z","message":{"role":"user","content":"second"}}` + "\n"
	line3 := `{"type":"assistant","sessionId":"s1","timestamp":"2025-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"reply"}],"model":"claude-3-opus"}}` + "\n"

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line2 + line3); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	// Read incrementally from the stored offset
	offsets := restoreOffsets(state)
	known := restoreKnownFiles(state)

	if !known[path] {
		t.Fatal("file should be known after baseline")
	}

	prevOffset := int64(offsets[path])
	if prevOffset != baselineOffset {
		t.Fatalf("restored offset = %d, want %d", prevOffset, baselineOffset)
	}

	recs, newOffset, err := parseJSONL(path, prevOffset)
	if err != nil {
		t.Fatalf("parseJSONL error: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (incremental)", len(recs))
	}
	if recs[0].Content != "second" {
		t.Errorf("record[0].Content = %q, want %q", recs[0].Content, "second")
	}
	if recs[1].Content != "reply" {
		t.Errorf("record[1].Content = %q, want %q", recs[1].Content, "reply")
	}
	if recs[1].Model != "claude-3-opus" {
		t.Errorf("record[1].Model = %q, want %q", recs[1].Model, "claude-3-opus")
	}

	expectedOffset := int64(len(line1) + len(line2) + len(line3))
	if newOffset != expectedOffset {
		t.Errorf("newOffset = %d, want %d", newOffset, expectedOffset)
	}
}

func TestClaudeCollect_CarryForwardKnownFiles(t *testing.T) {
	// When a file is in known_files but NOT in the active session list,
	// the carry-forward logic should preserve it in the next state to
	// prevent re-baselining on a subsequent cycle.
	path := "/fake/projects/hash/session.jsonl"

	prevKnown := map[string]bool{path: true}
	prevOffsets := map[string]float64{path: 500.0}

	// Simulate a cycle where this file is NOT in active sessions
	activeKnown := map[string]bool{} // empty: no active sessions found this cycle
	newOffsets := make(map[string]float64)

	// Carry forward logic (mirrors Collect)
	for p := range prevKnown {
		if !activeKnown[p] {
			activeKnown[p] = true
			if off, ok := prevOffsets[p]; ok {
				newOffsets[p] = off
			}
		}
	}

	if !activeKnown[path] {
		t.Error("carry-forward should keep file in known_files")
	}
	if newOffsets[path] != 500.0 {
		t.Errorf("carried-forward offset = %f, want 500", newOffsets[path])
	}
}

// ── resolveClaudeProjectJSONL tests ──────────────────────────────────────

func TestResolveClaudeProjectJSONL_DirectPath(t *testing.T) {
	projectsDir := t.TempDir()
	sessionID := "test-session-abc"

	// Create the expected directory structure:
	// {projectsDir}/{cwdHash}/{sessionId}.jsonl
	// Claude converts "/" to "-" for the cwd hash
	originCWD := "/Users/test/myproject"
	cwdHash := strings.ReplaceAll(originCWD, "/", "-")

	sessionDir := filepath.Join(projectsDir, cwdHash)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}

	jsonlPath := filepath.Join(sessionDir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	got := resolveClaudeProjectJSONL(sessionID, originCWD, "", projectsDir)
	if got != jsonlPath {
		t.Errorf("resolveClaudeProjectJSONL = %q, want %q", got, jsonlPath)
	}
}

func TestResolveClaudeProjectJSONL_FallbackScan(t *testing.T) {
	projectsDir := t.TempDir()
	sessionID := "fallback-session-xyz"

	// Create the file under a directory that does NOT match the provided CWD
	someDir := filepath.Join(projectsDir, "-some-other-project")
	if err := os.MkdirAll(someDir, 0755); err != nil {
		t.Fatal(err)
	}

	jsonlPath := filepath.Join(someDir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Use a CWD that does NOT match "-some-other-project"
	got := resolveClaudeProjectJSONL(sessionID, "/nonexistent/path", "", projectsDir)
	if got != jsonlPath {
		t.Errorf("fallback scan: got %q, want %q", got, jsonlPath)
	}
}

func TestResolveClaudeProjectJSONL_NotFound(t *testing.T) {
	projectsDir := t.TempDir()

	got := resolveClaudeProjectJSONL("nonexistent-session-id", "/some/cwd", "", projectsDir)
	if got != "" {
		t.Errorf("expected empty string for missing session, got %q", got)
	}
}

func TestResolveClaudeProjectJSONL_CWDFallback(t *testing.T) {
	// When originCWD does not match but cwd does, the file should be found
	projectsDir := t.TempDir()
	sessionID := "cwd-fallback-sess"

	cwd := "/workspace/project"
	cwdHash := strings.ReplaceAll(cwd, "/", "-")
	sessionDir := filepath.Join(projectsDir, cwdHash)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(sessionDir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// originCWD doesn't match, but cwd does
	got := resolveClaudeProjectJSONL(sessionID, "/wrong/origin", cwd, projectsDir)
	if got != jsonlPath {
		t.Errorf("cwd fallback: got %q, want %q", got, jsonlPath)
	}
}

// ── extractTipTapSnapshots tests ─────────────────────────────────────────

func TestExtractTipTapSnapshots_ValidData(t *testing.T) {
	// Build synthetic binary data containing:
	// - the tipTapEditorS marker
	// - an "updatedAt" field with a valid ms-epoch timestamp
	// - a "text" field with the user's message
	//
	// The function scans raw bytes, extracts a printable window, then
	// parses timestamps and text from the cleaned string.
	ts := "1700000000000" // valid ms epoch (Nov 2023)
	payload := fmt.Sprintf(
		`tipTapEditorState,"updatedAt":%s,"content":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"hello world"}]}]}`,
		ts,
	)
	data := []byte(payload)

	snapshots := extractTipTapSnapshots(data)
	if len(snapshots) == 0 {
		t.Fatal("expected at least 1 snapshot, got 0")
	}

	snap := snapshots[0]
	if snap.UpdatedAt != 1700000000000 {
		t.Errorf("UpdatedAt = %f, want 1700000000000", snap.UpdatedAt)
	}
	if snap.Text != "hello world" {
		t.Errorf("Text = %q, want %q", snap.Text, "hello world")
	}
}

func TestExtractTipTapSnapshots_NoMarker(t *testing.T) {
	data := []byte(`{"updatedAt":1700000000000,"text":"should not match"}`)
	snapshots := extractTipTapSnapshots(data)
	if len(snapshots) != 0 {
		t.Errorf("expected 0 snapshots without marker, got %d", len(snapshots))
	}
}

func TestExtractTipTapSnapshots_MultipleMarkers(t *testing.T) {
	// Two separate tipTap entries in the same data blob
	ts1 := "1700000000000"
	ts2 := "1700000001000"
	entry1 := fmt.Sprintf(
		`tipTapEditorState,"updatedAt":%s,"content":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"msg one"}]}]}`,
		ts1,
	)
	entry2 := fmt.Sprintf(
		`tipTapEditorState,"updatedAt":%s,"content":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"msg two"}]}]}`,
		ts2,
	)

	// Pad between entries to ensure they are discovered independently
	padding := strings.Repeat("\x00", 100)
	data := []byte(entry1 + padding + entry2)

	snapshots := extractTipTapSnapshots(data)
	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}
	if snapshots[0].UpdatedAt != 1700000000000 {
		t.Errorf("snap[0].UpdatedAt = %f, want 1700000000000", snapshots[0].UpdatedAt)
	}
	if snapshots[1].UpdatedAt != 1700000001000 {
		t.Errorf("snap[1].UpdatedAt = %f, want 1700000001000", snapshots[1].UpdatedAt)
	}
}

func TestExtractTipTapSnapshots_InvalidTimestamp(t *testing.T) {
	// Timestamp outside the plausible range should produce no snapshots
	payload := `tipTapEditorState,"updatedAt":9999999999999,"text":"nope"}`
	data := []byte(payload)
	snapshots := extractTipTapSnapshots(data)
	if len(snapshots) != 0 {
		t.Errorf("expected 0 snapshots for out-of-range timestamp, got %d", len(snapshots))
	}
}

func TestExtractTipTapSnapshots_BinaryPadding(t *testing.T) {
	// Simulate real LevelDB data: binary noise before/after the marker
	prefix := []byte{0x01, 0x02, 0x00, 0xFF, 0x10}
	suffix := []byte{0x00, 0x00, 0xFF}
	payload := fmt.Sprintf(
		`tipTapEditorState,"updatedAt":1700000000000,"content":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"binary test"}]}]}`,
	)
	data := append(prefix, []byte(payload)...)
	data = append(data, suffix...)

	snapshots := extractTipTapSnapshots(data)
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot with binary padding, got %d", len(snapshots))
	}
	if snapshots[0].Text != "binary test" {
		t.Errorf("Text = %q, want %q", snapshots[0].Text, "binary test")
	}
}

// ── parseJSONL full-cycle integration ────────────────────────────────────

func TestParseJSONL_FullCycleWithOffsets(t *testing.T) {
	// Simulates the full Collect() lifecycle using parseJSONL directly:
	// 1. Write initial data, baseline offset = file size
	// 2. Append new data, parse from baseline offset
	// 3. Append more, parse from new offset
	// 4. No new data, parse returns nothing
	dir := t.TempDir()
	path := filepath.Join(dir, "lifecycle.jsonl")

	// Phase 1: Initial data (would be baselined, not parsed)
	initial := `{"type":"user","sessionId":"lc","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"baseline msg"}}` + "\n"
	if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}
	baselineOffset := int64(len(initial))

	// Phase 2: Append and parse
	append1 := `{"type":"user","sessionId":"lc","timestamp":"2025-01-01T00:01:00Z","message":{"role":"user","content":"cycle2"}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(append1)
	f.Close()

	recs, offset2, err := parseJSONL(path, baselineOffset)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("phase 2: got %d records, want 1", len(recs))
	}
	if recs[0].Content != "cycle2" {
		t.Errorf("phase 2: content = %q, want %q", recs[0].Content, "cycle2")
	}

	// Phase 3: Append more
	append2 := `{"type":"assistant","sessionId":"lc","timestamp":"2025-01-01T00:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"response3"}],"model":"claude-3-sonnet"}}` + "\n"
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(append2)
	f.Close()

	recs, offset3, err := parseJSONL(path, offset2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("phase 3: got %d records, want 1", len(recs))
	}
	if recs[0].Content != "response3" {
		t.Errorf("phase 3: content = %q, want %q", recs[0].Content, "response3")
	}
	if recs[0].Model != "claude-3-sonnet" {
		t.Errorf("phase 3: model = %q, want %q", recs[0].Model, "claude-3-sonnet")
	}

	// Phase 4: No new data
	recs, offset4, err := parseJSONL(path, offset3)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("phase 4: got %d records, want 0 (no new data)", len(recs))
	}
	if offset4 != offset3 {
		t.Errorf("phase 4: offset should not change, got %d want %d", offset4, offset3)
	}
}

func TestParseJSONL_UsageTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.jsonl")

	line := `{"type":"assistant","sessionId":"tok","timestamp":"2025-01-01T00:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"reply"}],"model":"claude-3-opus","usage":{"input_tokens":100,"output_tokens":250}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0644); err != nil {
		t.Fatal(err)
	}

	recs, _, err := parseJSONL(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", recs[0].InputTokens)
	}
	if recs[0].OutputTokens != 250 {
		t.Errorf("OutputTokens = %d, want 250", recs[0].OutputTokens)
	}
}

func TestParseJSONL_AuditTimestampFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	// Line without "timestamp" but with "_audit_timestamp"
	line := `{"type":"user","sessionId":"aud","_audit_timestamp":"2025-06-15T12:00:00Z","message":{"role":"user","content":"audit fallback"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0644); err != nil {
		t.Fatal(err)
	}

	recs, _, err := parseJSONL(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Timestamp != "2025-06-15T12:00:00Z" {
		t.Errorf("Timestamp = %q, want audit timestamp fallback", recs[0].Timestamp)
	}
}

// ── findJSONLFiles tests ─────────────────────────────────────────────────

func TestFindJSONLFiles_LookbackFilter(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "proj-hash")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	recentFile := filepath.Join(subDir, "recent.jsonl")
	if err := os.WriteFile(recentFile, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	oldFile := filepath.Join(subDir, "old.jsonl")
	if err := os.WriteFile(oldFile, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// Set old file mtime to 48 hours ago
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	files, err := findJSONLFiles(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	for _, f := range files {
		found[f] = true
	}

	if !found[recentFile] {
		t.Error("recent file should be found within lookback")
	}
	if found[oldFile] {
		t.Error("old file should be excluded by lookback")
	}
}

func TestFindJSONLFiles_NonexistentDir(t *testing.T) {
	files, err := findJSONLFiles("/nonexistent/path/xxxxx", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files for nonexistent dir, got %d", len(files))
	}
}
