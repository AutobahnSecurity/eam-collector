package parsers

import (
	"strings"
	"testing"
)

func TestDetectSubmissions_NormalSubmission(t *testing.T) {
	// Text grows to 50 chars, then drops to 5 -> submitted
	snapshots := []tipTapSnapshot{
		{Text: "Hello", UpdatedAt: 1000},
		{Text: strings.Repeat("a", 50), UpdatedAt: 2000},
		{Text: "short", UpdatedAt: 3000}, // big drop -> submission
	}
	got := detectSubmissions(snapshots, 0)
	if len(got) != 1 {
		t.Fatalf("got %d submissions, want 1", len(got))
	}
	if got[0].Text != strings.Repeat("a", 50) {
		t.Errorf("submitted text = %q, want 50 a's", got[0].Text)
	}
	if got[0].SubmittedAt != 3000 {
		t.Errorf("SubmittedAt = %f, want 3000", got[0].SubmittedAt)
	}
}

func TestDetectSubmissions_MinorBackspace(t *testing.T) {
	// Text 50->45 is a minor edit, not a submission
	snapshots := []tipTapSnapshot{
		{Text: strings.Repeat("a", 50), UpdatedAt: 1000},
		{Text: strings.Repeat("a", 45), UpdatedAt: 2000}, // minor shrink
	}
	got := detectSubmissions(snapshots, 0)
	if len(got) != 0 {
		t.Errorf("got %d submissions, want 0 (minor backspace)", len(got))
	}
}

func TestDetectSubmissions_MultipleSubmissions(t *testing.T) {
	snapshots := []tipTapSnapshot{
		{Text: strings.Repeat("a", 30), UpdatedAt: 1000},
		{Text: "x", UpdatedAt: 2000},                     // submit #1
		{Text: strings.Repeat("b", 40), UpdatedAt: 3000},
		{Text: "y", UpdatedAt: 4000},                     // submit #2
	}
	got := detectSubmissions(snapshots, 0)
	if len(got) != 2 {
		t.Fatalf("got %d submissions, want 2", len(got))
	}
	if got[0].Text != strings.Repeat("a", 30) {
		t.Errorf("submit[0] text = %q, want 30 a's", got[0].Text)
	}
	if got[1].Text != strings.Repeat("b", 40) {
		t.Errorf("submit[1] text = %q, want 40 b's", got[1].Text)
	}
}

func TestDetectSubmissions_AfterTSFiltering(t *testing.T) {
	snapshots := []tipTapSnapshot{
		{Text: strings.Repeat("a", 30), UpdatedAt: 1000},
		{Text: "x", UpdatedAt: 2000},                     // drop at ts=2000
		{Text: strings.Repeat("b", 40), UpdatedAt: 3000},
		{Text: "y", UpdatedAt: 4000},                     // drop at ts=4000
	}
	// afterTS=2500: only the second submission (shrink at 4000) passes
	got := detectSubmissions(snapshots, 2500)
	if len(got) != 1 {
		t.Fatalf("got %d submissions, want 1 (afterTS filter)", len(got))
	}
	if got[0].Text != strings.Repeat("b", 40) {
		t.Errorf("submitted text = %q, want 40 b's", got[0].Text)
	}
}

func TestDetectSubmissions_PeakExactlyMinLen(t *testing.T) {
	// Peak exactly at minSubmissionLen (10 chars) should NOT be submitted
	// because the check is len(peak.Text) > minSubmissionLen (strictly greater)
	snapshots := []tipTapSnapshot{
		{Text: strings.Repeat("a", 10), UpdatedAt: 1000},
		{Text: "x", UpdatedAt: 2000},
	}
	got := detectSubmissions(snapshots, 0)
	if len(got) != 0 {
		t.Errorf("got %d submissions, want 0 (peak exactly at minSubmissionLen)", len(got))
	}
}

func TestDetectSubmissions_PeakJustAboveMinLen(t *testing.T) {
	// Peak at 11 chars (just above minSubmissionLen) should be submitted
	snapshots := []tipTapSnapshot{
		{Text: strings.Repeat("a", 11), UpdatedAt: 1000},
		{Text: "x", UpdatedAt: 2000},
	}
	got := detectSubmissions(snapshots, 0)
	if len(got) != 1 {
		t.Fatalf("got %d submissions, want 1", len(got))
	}
}

func TestDetectSubmissions_EmptySnapshots(t *testing.T) {
	got := detectSubmissions(nil, 0)
	if len(got) != 0 {
		t.Errorf("got %d submissions, want 0", len(got))
	}
}

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		name string
		input string
		want float64
	}{
		{
			name:  "valid 13-digit ms epoch",
			input: "1700000000000",
			want:  1700000000000,
		},
		{
			name:  "out of range before 2020",
			input: "1500000000000",
			want:  0,
		},
		{
			name:  "out of range after 2040",
			input: "2300000000000",
			want:  0,
		},
		{
			name:  "empty string",
			input: "",
			want:  0,
		},
		{
			name:  "non-numeric",
			input: "abc",
			want:  0,
		},
		{
			name:  "trailing non-digits stripped",
			input: "1700000000000xyz",
			want:  1700000000000,
		},
		{
			name:  "boundary lower 2020-01-01",
			input: "1577836800000",
			want:  1577836800000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTimestamp(tt.input)
			if got != tt.want {
				t.Errorf("parseTimestamp(%q) = %f, want %f", tt.input, got, tt.want)
			}
		})
	}
}

func TestCleanToPrintable(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{
			name:  "normal printable text",
			input: []byte("Hello World 123!"),
			want:  "Hello World 123!",
		},
		{
			name:  "non-printable bytes stripped",
			input: []byte("Hello\x00\x01\x02World\x7F"),
			want:  "HelloWorld",
		},
		{
			name:  "empty input",
			input: []byte{},
			want:  "",
		},
		{
			name:  "all non-printable",
			input: []byte{0x00, 0x01, 0x1F, 0x7F},
			want:  "",
		},
		{
			name:  "space and tilde boundaries",
			input: []byte{31, 32, 126, 127}, // 31=non-printable, 32=space, 126=tilde, 127=DEL
			want:  " ~",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanToPrintable(tt.input)
			if got != tt.want {
				t.Errorf("cleanToPrintable(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractTipTapText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "normal pattern",
			input: `something text","key":"actual content"}]}`,
			want:  "actual content",
		},
		{
			name:  "quoted text field",
			input: `something "text":"hello world"}`,
			want:  "hello world",
		},
		{
			name:  "missing text field",
			input: `something without the keyword`,
			want:  "",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "text field with no colon-quote value",
			input: `text",novalue`,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTipTapText(tt.input)
			if got != tt.want {
				t.Errorf("extractTipTapText(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
