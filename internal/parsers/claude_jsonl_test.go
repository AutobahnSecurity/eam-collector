package parsers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractContent_StringContent(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	got := ExtractContent(raw)
	if got != "hello world" {
		t.Errorf("ExtractContent(string) = %q, want %q", got, "hello world")
	}
}

func TestExtractContent_ArrayWithTextBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"hi"}]`)
	got := ExtractContent(raw)
	if got != "hi" {
		t.Errorf("ExtractContent(text block) = %q, want %q", got, "hi")
	}
}

func TestExtractContent_ToolResultStringContent(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_result","content":"tool output","tool_use_id":"123"}]`)
	got := ExtractContent(raw)
	if got != "tool output" {
		t.Errorf("ExtractContent(tool_result string) = %q, want %q", got, "tool output")
	}
}

func TestExtractContent_ToolResultNestedBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_result","content":[{"type":"text","text":"nested result"}],"tool_use_id":"456"}]`)
	got := ExtractContent(raw)
	if got != "nested result" {
		t.Errorf("ExtractContent(tool_result nested) = %q, want %q", got, "nested result")
	}
}

func TestExtractContent_EmptyAndNil(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		got := ExtractContent(nil)
		if got != "" {
			t.Errorf("ExtractContent(nil) = %q, want empty", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		got := ExtractContent(json.RawMessage{})
		if got != "" {
			t.Errorf("ExtractContent(empty) = %q, want empty", got)
		}
	})
}

func TestExtractContent_NullBytesStripped(t *testing.T) {
	raw := json.RawMessage(`"hello\u0000world"`)
	got := ExtractContent(raw)
	if got != "helloworld" {
		t.Errorf("ExtractContent(null bytes) = %q, want %q", got, "helloworld")
	}
}

func TestExtractContent_MultipleTextBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`)
	got := ExtractContent(raw)
	if got != "first\nsecond" {
		t.Errorf("ExtractContent(multi text) = %q, want %q", got, "first\nsecond")
	}
}

func TestExtractContent_ToolUseBlockIgnored(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_use","id":"x","name":"bash","input":{}}]`)
	got := ExtractContent(raw)
	if got != "" {
		t.Errorf("ExtractContent(tool_use only) = %q, want empty", got)
	}
}

func TestParseJSONL_BasicRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	lines := []string{
		`{"type":"user","sessionId":"s1","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","sessionId":"s1","timestamp":"2024-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hi there"}],"model":"claude-3-opus"}}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	records, newOffset, err := parseJSONL(path, 0)
	if err != nil {
		t.Fatalf("parseJSONL error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].Role != "user" || records[0].Content != "hello" {
		t.Errorf("record[0] = %+v, want role=user content=hello", records[0])
	}
	if records[1].Role != "assistant" || records[1].Content != "hi there" {
		t.Errorf("record[1] = %+v, want role=assistant content='hi there'", records[1])
	}
	if records[1].Model != "claude-3-opus" {
		t.Errorf("record[1].Model = %q, want %q", records[1].Model, "claude-3-opus")
	}
	if newOffset != int64(len(content)) {
		t.Errorf("newOffset = %d, want %d", newOffset, len(content))
	}
}

func TestParseJSONL_OffsetResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	line1 := `{"type":"user","sessionId":"s1","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"first"}}` + "\n"
	line2 := `{"type":"user","sessionId":"s1","timestamp":"2024-01-01T00:00:01Z","message":{"role":"user","content":"second"}}` + "\n"
	if err := os.WriteFile(path, []byte(line1+line2), 0644); err != nil {
		t.Fatal(err)
	}

	// Parse from offset past the first line
	offset := int64(len(line1))
	records, newOffset, err := parseJSONL(path, offset)
	if err != nil {
		t.Fatalf("parseJSONL error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Content != "second" {
		t.Errorf("record content = %q, want %q", records[0].Content, "second")
	}
	if newOffset != int64(len(line1)+len(line2)) {
		t.Errorf("newOffset = %d, want %d", newOffset, len(line1)+len(line2))
	}
}

func TestParseJSONL_OversizedLineSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	normal := `{"type":"user","sessionId":"s1","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"ok"}}` + "\n"
	// Create a line larger than 1 MB
	bigContent := strings.Repeat("x", 1024*1024+100)
	oversized := `{"type":"user","sessionId":"s1","timestamp":"2024-01-01T00:00:01Z","message":{"role":"user","content":"` + bigContent + `"}}` + "\n"
	after := `{"type":"user","sessionId":"s1","timestamp":"2024-01-01T00:00:02Z","message":{"role":"user","content":"after"}}` + "\n"

	if err := os.WriteFile(path, []byte(normal+oversized+after), 0644); err != nil {
		t.Fatal(err)
	}

	records, _, err := parseJSONL(path, 0)
	if err != nil {
		t.Fatalf("parseJSONL error: %v", err)
	}
	// Should get "ok" and "after", oversized line skipped
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (oversized skipped)", len(records))
	}
	if records[0].Content != "ok" {
		t.Errorf("record[0].Content = %q, want %q", records[0].Content, "ok")
	}
	if records[1].Content != "after" {
		t.Errorf("record[1].Content = %q, want %q", records[1].Content, "after")
	}
}

func TestParseJSONL_IncompleteTrailingLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	complete := `{"type":"user","sessionId":"s1","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"done"}}` + "\n"
	incomplete := `{"type":"user","sessionId":"s1","timestamp":"2024-01-01T00`

	if err := os.WriteFile(path, []byte(complete+incomplete), 0644); err != nil {
		t.Fatal(err)
	}

	records, newOffset, err := parseJSONL(path, 0)
	if err != nil {
		t.Fatalf("parseJSONL error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1 (incomplete line left)", len(records))
	}
	if records[0].Content != "done" {
		t.Errorf("record.Content = %q, want %q", records[0].Content, "done")
	}
	// newOffset should be at end of the complete line, NOT end of file
	if newOffset != int64(len(complete)) {
		t.Errorf("newOffset = %d, want %d (should stop at last newline)", newOffset, len(complete))
	}
}

func TestParseJSONL_SystemTypeSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	content := `{"type":"system","sessionId":"s1","timestamp":"2024-01-01T00:00:00Z","message":{"role":"system","content":"sys"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	records, _, err := parseJSONL(path, 0)
	if err != nil {
		t.Fatalf("parseJSONL error: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("got %d records, want 0 (system type skipped)", len(records))
	}
}

func TestParseJSONL_SessionIDPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	content := `{"type":"user","sessionId":"abc123","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	records, _, err := parseJSONL(path, 0)
	if err != nil {
		t.Fatalf("parseJSONL error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].SessionID != "collector:claude:abc123" {
		t.Errorf("SessionID = %q, want %q", records[0].SessionID, "collector:claude:abc123")
	}
}

func TestParseJSONL_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	records, newOffset, err := parseJSONL(path, 0)
	if err != nil {
		t.Fatalf("parseJSONL error: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("got %d records, want 0", len(records))
	}
	if newOffset != 0 {
		t.Errorf("newOffset = %d, want 0", newOffset)
	}
}

func TestParseJSONL_SyntheticModelCleared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	content := `{"type":"assistant","sessionId":"s1","timestamp":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"reply"}],"model":"<synthetic>"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	records, _, err := parseJSONL(path, 0)
	if err != nil {
		t.Fatalf("parseJSONL error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Model != "" {
		t.Errorf("Model = %q, want empty (synthetic cleared)", records[0].Model)
	}
}
