package parsers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// jsonRoundTrip simulates the JSON marshal/unmarshal cycle that happens in
// production when state is persisted to disk between Collect calls.
// Without this, typed Go maps (map[string]float64, []string, etc.) returned
// by Collect would not match the type assertions in the restore functions,
// which expect map[string]any and []any (the types json.Unmarshal produces).
func jsonRoundTrip(state map[string]any) map[string]any {
	data, err := json.Marshal(state)
	if err != nil {
		panic(fmt.Sprintf("jsonRoundTrip marshal: %v", err))
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("jsonRoundTrip unmarshal: %v", err))
	}
	return out
}

// ── Copilot per-message dedup tests ──────────────────────────────────────

func TestCopilotCollect_IncrementalRecords(t *testing.T) {
	// Directory structure must match the glob: {baseDir}/{workspace}/chatSessions/{file}.json
	baseDir := t.TempDir()
	workspace := filepath.Join(baseDir, "workspace1", "chatSessions")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}

	// Create initial session with 2 requests
	session := copilotSession{
		SessionID:       "copilot-sess-1",
		CreationDate:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		LastMessageDate: time.Date(2025, 1, 1, 0, 5, 0, 0, time.UTC).UnixMilli(),
		Requests: []struct {
			RequestID string `json:"requestId"`
			Message   struct {
				Text string `json:"text"`
			} `json:"message"`
			Response []struct {
				Value string `json:"value"`
			} `json:"response"`
		}{
			{
				RequestID: "req1",
				Message:   struct{ Text string `json:"text"` }{Text: "first question"},
				Response: []struct{ Value string `json:"value"` }{
					{Value: "first answer"},
				},
			},
			{
				RequestID: "req2",
				Message:   struct{ Text string `json:"text"` }{Text: "second question"},
				Response: []struct{ Value string `json:"value"` }{
					{Value: "second answer"},
				},
			},
		},
	}

	sessionPath := filepath.Join(workspace, "session1.json")
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	parser := &CopilotParser{
		baseDir:  baseDir,
		lookback: 24 * time.Hour,
	}

	// First Collect: should return all 4 records (2 user + 2 assistant)
	recs1, state1, err := parser.Collect(nil)
	if err != nil {
		t.Fatalf("first Collect error: %v", err)
	}
	if len(recs1) != 4 {
		t.Fatalf("first Collect: got %d records, want 4", len(recs1))
	}

	// Verify record content
	if recs1[0].Role != "user" || recs1[0].Content != "first question" {
		t.Errorf("recs1[0] = role=%q content=%q, want user/first question", recs1[0].Role, recs1[0].Content)
	}
	if recs1[1].Role != "assistant" || recs1[1].Content != "first answer" {
		t.Errorf("recs1[1] = role=%q content=%q, want assistant/first answer", recs1[1].Role, recs1[1].Content)
	}
	if recs1[0].Source != "copilot" {
		t.Errorf("source = %q, want copilot", recs1[0].Source)
	}
	if recs1[0].AIVendor != "GitHub" {
		t.Errorf("AIVendor = %q, want GitHub", recs1[0].AIVendor)
	}

	// Now add a 3rd request to the session file (simulating new message)
	session.Requests = append(session.Requests, struct {
		RequestID string `json:"requestId"`
		Message   struct {
			Text string `json:"text"`
		} `json:"message"`
		Response []struct {
			Value string `json:"value"`
		} `json:"response"`
	}{
		RequestID: "req3",
		Message:   struct{ Text string `json:"text"` }{Text: "third question"},
		Response: []struct{ Value string `json:"value"` }{
			{Value: "third answer"},
		},
	})
	session.LastMessageDate = time.Date(2025, 1, 1, 0, 10, 0, 0, time.UTC).UnixMilli()

	data, err = json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Touch the file to ensure mtime changes (some filesystems have 1s resolution)
	newTime := time.Now().Add(1 * time.Second)
	if err := os.Chtimes(sessionPath, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	// Roundtrip state through JSON to match production behavior
	state1rt := jsonRoundTrip(state1)

	// Verify request_counts survived the roundtrip
	reqCountsRaw, ok := state1rt["request_counts"]
	if !ok {
		t.Fatal("state missing request_counts after roundtrip")
	}
	reqCounts, ok := reqCountsRaw.(map[string]any)
	if !ok {
		t.Fatalf("request_counts is %T, want map[string]any", reqCountsRaw)
	}
	if cnt, ok := reqCounts[sessionPath].(float64); !ok || cnt != 4 {
		t.Errorf("request_counts[%s] = %v, want 4", sessionPath, reqCounts[sessionPath])
	}

	// Second Collect with roundtripped state: should return ONLY the new request's records
	recs2, state2, err := parser.Collect(state1rt)
	if err != nil {
		t.Fatalf("second Collect error: %v", err)
	}
	if len(recs2) != 2 {
		t.Fatalf("second Collect: got %d records, want 2 (only new request)", len(recs2))
	}
	if recs2[0].Content != "third question" {
		t.Errorf("recs2[0].Content = %q, want %q", recs2[0].Content, "third question")
	}
	if recs2[1].Content != "third answer" {
		t.Errorf("recs2[1].Content = %q, want %q", recs2[1].Content, "third answer")
	}

	// Verify updated request_counts after roundtrip
	state2rt := jsonRoundTrip(state2)
	reqCountsRaw2 := state2rt["request_counts"]
	reqCounts2, _ := reqCountsRaw2.(map[string]any)
	if cnt, ok := reqCounts2[sessionPath].(float64); !ok || cnt != 6 {
		t.Errorf("request_counts after update = %v, want 6", reqCounts2[sessionPath])
	}
}

func TestCopilotCollect_UnchangedFile(t *testing.T) {
	// When the file mtime hasn't changed, Collect should return 0 records.
	baseDir := t.TempDir()
	workspace := filepath.Join(baseDir, "ws", "chatSessions")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}

	session := copilotSession{
		SessionID:       "copilot-unchanged",
		CreationDate:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		LastMessageDate: time.Date(2025, 1, 1, 0, 1, 0, 0, time.UTC).UnixMilli(),
		Requests: []struct {
			RequestID string `json:"requestId"`
			Message   struct {
				Text string `json:"text"`
			} `json:"message"`
			Response []struct {
				Value string `json:"value"`
			} `json:"response"`
		}{
			{
				RequestID: "r1",
				Message:   struct{ Text string `json:"text"` }{Text: "hello"},
				Response: []struct{ Value string `json:"value"` }{
					{Value: "hi"},
				},
			},
		},
	}

	sessionPath := filepath.Join(workspace, "unchanged.json")
	data, _ := json.Marshal(session)
	if err := os.WriteFile(sessionPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	parser := &CopilotParser{
		baseDir:  baseDir,
		lookback: 24 * time.Hour,
	}

	// First Collect
	_, state1, err := parser.Collect(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Roundtrip state to match production JSON serialization
	state1rt := jsonRoundTrip(state1)

	// Second Collect with roundtripped state and unchanged file
	recs2, _, err := parser.Collect(state1rt)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs2) != 0 {
		t.Errorf("unchanged file: got %d records, want 0", len(recs2))
	}
}

func TestCopilotCollect_EmptyBaseDir(t *testing.T) {
	parser := &CopilotParser{
		baseDir:  "/nonexistent/copilot/path",
		lookback: 24 * time.Hour,
	}

	recs, state, err := parser.Collect(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("nonexistent dir: got %d records, want 0", len(recs))
	}
	// State should be returned as-is (nil prevState)
	if state != nil {
		t.Errorf("expected nil state for nonexistent dir, got %v", state)
	}
}

func TestCopilotCollect_SessionIDPrefix(t *testing.T) {
	baseDir := t.TempDir()
	workspace := filepath.Join(baseDir, "ws", "chatSessions")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}

	session := copilotSession{
		SessionID:       "my-session-id",
		CreationDate:    time.Now().UnixMilli(),
		LastMessageDate: time.Now().UnixMilli(),
		Requests: []struct {
			RequestID string `json:"requestId"`
			Message   struct {
				Text string `json:"text"`
			} `json:"message"`
			Response []struct {
				Value string `json:"value"`
			} `json:"response"`
		}{
			{
				RequestID: "r1",
				Message:   struct{ Text string `json:"text"` }{Text: "test"},
				Response:  nil,
			},
		},
	}

	data, _ := json.Marshal(session)
	if err := os.WriteFile(filepath.Join(workspace, "s.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	parser := &CopilotParser{baseDir: baseDir, lookback: 24 * time.Hour}
	recs, _, err := parser.Collect(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 1 {
		t.Fatal("expected at least 1 record")
	}
	want := "collector:copilot:my-session-id"
	if recs[0].SessionID != want {
		t.Errorf("SessionID = %q, want %q", recs[0].SessionID, want)
	}
}

// ── Continue.dev tests ───────────────────────────────────────────────────

func TestContinueCollect_DeterministicTruncation(t *testing.T) {
	// Build a state with 5001 processed session IDs. Collect (with no new files)
	// should truncate to exactly 5000, keeping the lexicographically last 5000.
	baseDir := t.TempDir() // empty dir, no session files

	// State must use []any (the type json.Unmarshal produces) for the restore
	// function to recognize the values.
	ids := make([]any, 5001)
	for i := 0; i < 5001; i++ {
		ids[i] = fmt.Sprintf("session-%05d", i)
	}

	prevState := map[string]any{
		"processed": ids,
	}

	parser := &ContinueParser{
		baseDir:  baseDir,
		lookback: 24 * time.Hour,
	}

	_, state1, err := parser.Collect(prevState)
	if err != nil {
		t.Fatalf("Collect error: %v", err)
	}

	// Check the truncated processed list
	processedRaw, ok := state1["processed"]
	if !ok {
		t.Fatal("state missing processed key")
	}
	processed, ok := processedRaw.([]string)
	if !ok {
		t.Fatalf("processed is %T, want []string", processedRaw)
	}
	if len(processed) != 5000 {
		t.Fatalf("truncated processed len = %d, want 5000", len(processed))
	}

	// Verify the list is sorted
	if !sort.StringsAreSorted(processed) {
		t.Error("processed list should be sorted")
	}

	// The truncation keeps the last 5000 (largest by sort order).
	// "session-00000" should be dropped (smallest).
	if processed[0] == "session-00000" {
		t.Error("session-00000 should have been truncated (oldest by sort)")
	}
	// "session-05000" should be present (largest)
	if processed[len(processed)-1] != "session-05000" {
		t.Errorf("last element = %q, want session-05000", processed[len(processed)-1])
	}

	// Roundtrip state through JSON to simulate production persistence
	state1rt := jsonRoundTrip(state1)

	// Call again with roundtripped state: no IDs should be lost (stable truncation)
	_, state2, err := parser.Collect(state1rt)
	if err != nil {
		t.Fatal(err)
	}
	processed2, ok := state2["processed"].([]string)
	if !ok {
		t.Fatalf("second processed is %T, want []string", state2["processed"])
	}
	if len(processed2) != 5000 {
		t.Fatalf("second truncation: len = %d, want 5000", len(processed2))
	}

	// Verify stability: same IDs after re-truncation
	for i, id := range processed {
		if processed2[i] != id {
			t.Errorf("processed[%d] changed from %q to %q after re-truncation", i, id, processed2[i])
			break
		}
	}
}

func TestContinueCollect_BasicSession(t *testing.T) {
	// Create a temp sessions dir with a JSON file containing a session.
	// Structure: {baseDir}/{sessionId}.json
	baseDir := t.TempDir()

	session := continueSession{
		SessionID: "continue-sess-001",
		Title:     "Test Session",
		History: []struct {
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}{
			{
				Message: struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}{
					Role:    "user",
					Content: json.RawMessage(`"what is Go?"`),
				},
			},
			{
				Message: struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}{
					Role:    "assistant",
					Content: json.RawMessage(`"Go is a programming language."`),
				},
			},
		},
	}

	sessionPath := filepath.Join(baseDir, "continue-sess-001.json")
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	parser := &ContinueParser{
		baseDir:  baseDir,
		lookback: 24 * time.Hour,
	}

	// First Collect: should return 2 records
	recs1, state1, err := parser.Collect(nil)
	if err != nil {
		t.Fatalf("first Collect error: %v", err)
	}
	if len(recs1) != 2 {
		t.Fatalf("first Collect: got %d records, want 2", len(recs1))
	}
	if recs1[0].Role != "user" || recs1[0].Content != "what is Go?" {
		t.Errorf("recs1[0] = role=%q content=%q, want user/'what is Go?'", recs1[0].Role, recs1[0].Content)
	}
	if recs1[1].Role != "assistant" || recs1[1].Content != "Go is a programming language." {
		t.Errorf("recs1[1] = role=%q content=%q", recs1[1].Role, recs1[1].Content)
	}
	if recs1[0].Source != "continuedev" {
		t.Errorf("source = %q, want continuedev", recs1[0].Source)
	}
	if recs1[0].AIVendor != "Continue" {
		t.Errorf("AIVendor = %q, want Continue", recs1[0].AIVendor)
	}
	if recs1[0].SessionID != "collector:continuedev:continue-sess-001" {
		t.Errorf("SessionID = %q, want collector:continuedev:continue-sess-001", recs1[0].SessionID)
	}

	// Verify session is in processed set
	processedRaw, ok := state1["processed"]
	if !ok {
		t.Fatal("state missing processed key")
	}
	processed, ok := processedRaw.([]string)
	if !ok {
		t.Fatalf("processed is %T, want []string", processedRaw)
	}
	found := false
	for _, id := range processed {
		if id == "continue-sess-001" {
			found = true
			break
		}
	}
	if !found {
		t.Error("session ID should be in processed set")
	}

	// Roundtrip state through JSON to simulate production persistence
	state1rt := jsonRoundTrip(state1)

	// Second Collect with roundtripped state: should return 0 records (already processed)
	recs2, _, err := parser.Collect(state1rt)
	if err != nil {
		t.Fatalf("second Collect error: %v", err)
	}
	if len(recs2) != 0 {
		t.Errorf("second Collect: got %d records, want 0 (already processed)", len(recs2))
	}
}

func TestContinueCollect_SessionsJsonSkipped(t *testing.T) {
	// The index file "sessions.json" should be ignored
	baseDir := t.TempDir()

	// Create a sessions.json index file with valid session content
	indexContent := `{"sessionId":"should-skip","history":[{"message":{"role":"user","content":"skip me"}}]}`
	if err := os.WriteFile(filepath.Join(baseDir, "sessions.json"), []byte(indexContent), 0644); err != nil {
		t.Fatal(err)
	}

	parser := &ContinueParser{baseDir: baseDir, lookback: 24 * time.Hour}
	recs, _, err := parser.Collect(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("sessions.json should be skipped, got %d records", len(recs))
	}
}

func TestContinueCollect_EmptySessionID(t *testing.T) {
	// Sessions without a sessionId should be skipped
	baseDir := t.TempDir()

	session := continueSession{
		SessionID: "",
		History: []struct {
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}{
			{
				Message: struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}{
					Role:    "user",
					Content: json.RawMessage(`"ignored"`),
				},
			},
		},
	}

	data, _ := json.Marshal(session)
	if err := os.WriteFile(filepath.Join(baseDir, "empty-id.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	parser := &ContinueParser{baseDir: baseDir, lookback: 24 * time.Hour}
	recs, _, err := parser.Collect(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("empty sessionId should be skipped, got %d records", len(recs))
	}
}

func TestContinueCollect_NonexistentDir(t *testing.T) {
	parser := &ContinueParser{
		baseDir:  "/nonexistent/continue/sessions",
		lookback: 24 * time.Hour,
	}

	recs, state, err := parser.Collect(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("nonexistent dir: got %d records, want 0", len(recs))
	}
	// State should be returned as-is (nil prevState)
	if state != nil {
		t.Errorf("expected nil state for nonexistent dir, got %v", state)
	}
}

func TestContinueCollect_ArrayContent(t *testing.T) {
	// Continue.dev content can be an array of content parts
	baseDir := t.TempDir()

	session := continueSession{
		SessionID: "array-content-sess",
		History: []struct {
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}{
			{
				Message: struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}{
					Role:    "user",
					Content: json.RawMessage(`[{"type":"text","text":"array message"}]`),
				},
			},
		},
	}

	data, _ := json.Marshal(session)
	if err := os.WriteFile(filepath.Join(baseDir, "array.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	parser := &ContinueParser{baseDir: baseDir, lookback: 24 * time.Hour}
	recs, _, err := parser.Collect(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Content != "array message" {
		t.Errorf("Content = %q, want %q", recs[0].Content, "array message")
	}
}

func TestContinueCollect_SystemRoleSkipped(t *testing.T) {
	// Messages with role "system" should be skipped
	baseDir := t.TempDir()

	session := continueSession{
		SessionID: "system-role-sess",
		History: []struct {
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}{
			{
				Message: struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}{
					Role:    "system",
					Content: json.RawMessage(`"system prompt"`),
				},
			},
			{
				Message: struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}{
					Role:    "user",
					Content: json.RawMessage(`"real question"`),
				},
			},
		},
	}

	data, _ := json.Marshal(session)
	if err := os.WriteFile(filepath.Join(baseDir, "system.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	parser := &ContinueParser{baseDir: baseDir, lookback: 24 * time.Hour}
	recs, _, err := parser.Collect(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (system role skipped)", len(recs))
	}
	if recs[0].Role != "user" {
		t.Errorf("expected user role, got %q", recs[0].Role)
	}
}
