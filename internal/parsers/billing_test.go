package parsers

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestExtractJSON_SimpleObject(t *testing.T) {
	input := `{"key":"value"}`
	got := extractJSON(input, 0)
	if got != `{"key":"value"}` {
		t.Errorf("extractJSON simple = %q, want %q", got, `{"key":"value"}`)
	}
}

func TestExtractJSON_NestedObject(t *testing.T) {
	input := `{"a":{"b":1}}`
	got := extractJSON(input, 0)
	if got != `{"a":{"b":1}}` {
		t.Errorf("extractJSON nested = %q, want %q", got, `{"a":{"b":1}}`)
	}
}

func TestExtractJSON_StringContainingBrace(t *testing.T) {
	// CRITICAL: This was the bug — braces inside string values must be handled correctly
	input := `{"key":"value with } brace"}`
	got := extractJSON(input, 0)
	if got != `{"key":"value with } brace"}` {
		t.Errorf("extractJSON string-with-brace = %q, want %q", got, `{"key":"value with } brace"}`)
	}
}

func TestExtractJSON_NoClosingBrace(t *testing.T) {
	input := `{"key":"value`
	got := extractJSON(input, 0)
	if got != "" {
		t.Errorf("extractJSON no-close = %q, want empty", got)
	}
}

func TestExtractJSON_NonZeroStart(t *testing.T) {
	input := `garbage{"key":"value"}`
	got := extractJSON(input, 7)
	if got != `{"key":"value"}` {
		t.Errorf("extractJSON offset = %q, want %q", got, `{"key":"value"}`)
	}
}

func TestExtractJSON_DeeplyNested(t *testing.T) {
	input := `{"a":{"b":{"c":{"d":42}}}}`
	got := extractJSON(input, 0)
	if got != input {
		t.Errorf("extractJSON deeply-nested = %q, want %q", got, input)
	}
}

func TestExtractJSON_ArrayValues(t *testing.T) {
	input := `{"arr":[1,2,3],"obj":{"x":true}}`
	got := extractJSON(input, 0)
	if got != input {
		t.Errorf("extractJSON array = %q, want %q", got, input)
	}
}

func TestExtractJSON_EscapedQuotesInString(t *testing.T) {
	input := `{"key":"value with \"escaped\" quotes"}`
	got := extractJSON(input, 0)
	if got != input {
		t.Errorf("extractJSON escaped-quotes = %q, want %q", got, input)
	}
}

func TestExtractJSON_TrailingContent(t *testing.T) {
	// extractJSON should stop at the end of the first JSON object
	input := `{"first":"obj"}{"second":"obj"}`
	got := extractJSON(input, 0)
	if got != `{"first":"obj"}` {
		t.Errorf("extractJSON trailing = %q, want %q", got, `{"first":"obj"}`)
	}
}

func TestExtractJSON_PrefixThenObject(t *testing.T) {
	// Simulates real cache data: URL header then JSON body
	input := `HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{"seat_tier_quantities":{"team_standard":21}}`
	start := 58 // position of {
	// Find actual start
	for i := 0; i < len(input); i++ {
		if input[i] == '{' {
			start = i
			break
		}
	}
	got := extractJSON(input, start)
	if got != `{"seat_tier_quantities":{"team_standard":21}}` {
		t.Errorf("extractJSON with-prefix = %q, want JSON object", got)
	}
}

func TestExtractJSON_EmptyObject(t *testing.T) {
	input := `{}`
	got := extractJSON(input, 0)
	if got != `{}` {
		t.Errorf("extractJSON empty-obj = %q, want %q", got, `{}`)
	}
}

func TestExtractJSON_MultipleBracesInStrings(t *testing.T) {
	// Multiple braces inside string values
	input := `{"a":"}{","b":"}{}"}`
	got := extractJSON(input, 0)
	if got != input {
		t.Errorf("extractJSON multi-brace-strings = %q, want %q", got, input)
	}
}

// --- decompressZstdBody tests ---

func TestDecompressZstdBody_ValidZstd(t *testing.T) {
	// Compress a known string with zstd
	original := `{"seat_tier_quantities":{"team_standard":21}}`
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := w.Write([]byte(original)); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}

	// Prepend some fake HTTP-header bytes before the zstd data
	header := []byte("HTTP/1.1 200 OK\r\nContent-Encoding: zstd\r\n\r\n")
	data := append(header, buf.Bytes()...)

	got := decompressZstdBody(data)
	if got != original {
		t.Errorf("decompressZstdBody valid = %q, want %q", got, original)
	}
}

func TestDecompressZstdBody_NoMagicBytes(t *testing.T) {
	data := []byte("just some plain text without any zstd content")
	got := decompressZstdBody(data)
	if got != "" {
		t.Errorf("decompressZstdBody no-magic = %q, want empty", got)
	}
}

func TestDecompressZstdBody_EmptyData(t *testing.T) {
	got := decompressZstdBody([]byte{})
	if got != "" {
		t.Errorf("decompressZstdBody empty = %q, want empty", got)
	}
}

// --- parseMembersLimit tests ---

func TestParseMembersLimit_ValidResponse(t *testing.T) {
	body := `some header junk {"seat_tier_quantities": {"team_standard": 21, "team_tier_1": 2}, "minimum_seats": 5, "members_limit": 25}`
	bd := BillingData{}
	parseMembersLimit(body, &bd)

	if len(bd.SeatTiers) != 2 {
		t.Fatalf("parseMembersLimit: got %d tiers, want 2", len(bd.SeatTiers))
	}

	total := 0
	for _, st := range bd.SeatTiers {
		total += st.Count
	}
	if total != 23 {
		t.Errorf("parseMembersLimit: tier sum = %d, want 23", total)
	}
	if bd.TotalSeats != 23 {
		t.Errorf("parseMembersLimit: TotalSeats = %d, want 23", bd.TotalSeats)
	}
}

func TestParseMembersLimit_NoJSON(t *testing.T) {
	body := "this body has no matching JSON at all"
	bd := BillingData{}
	parseMembersLimit(body, &bd)

	if len(bd.SeatTiers) != 0 {
		t.Errorf("parseMembersLimit no-json: got %d tiers, want 0", len(bd.SeatTiers))
	}
	if bd.TotalSeats != 0 {
		t.Errorf("parseMembersLimit no-json: TotalSeats = %d, want 0", bd.TotalSeats)
	}
}

// --- parseMembersCount tests ---

func TestParseMembersCount_ValidResponse(t *testing.T) {
	body := `prefix text {"total": 10, "by_seat_tier": {"standard": 8, "unassigned": 2}}`
	bd := BillingData{}
	parseMembersCount(body, &bd)

	// "unassigned" is filtered out, so only "standard" should remain
	if len(bd.SeatTiers) != 1 {
		t.Fatalf("parseMembersCount: got %d tiers, want 1", len(bd.SeatTiers))
	}
	if bd.SeatTiers[0].Tier != "standard" || bd.SeatTiers[0].Count != 8 {
		t.Errorf("parseMembersCount: tier = %+v, want {standard, 8}", bd.SeatTiers[0])
	}
	if bd.TotalSeats != 8 {
		t.Errorf("parseMembersCount: TotalSeats = %d, want 8", bd.TotalSeats)
	}
}

func TestParseMembersCount_SkipsIfTiersAlreadySet(t *testing.T) {
	body := `{"total": 10, "by_seat_tier": {"standard": 8}}`
	bd := BillingData{
		SeatTiers:  []SeatTierCount{{Tier: "existing", Count: 5}},
		TotalSeats: 5,
	}
	parseMembersCount(body, &bd)

	// Should not overwrite existing tiers
	if len(bd.SeatTiers) != 1 || bd.SeatTiers[0].Tier != "existing" {
		t.Errorf("parseMembersCount skip: tiers = %+v, want [{existing 5}]", bd.SeatTiers)
	}
	if bd.TotalSeats != 5 {
		t.Errorf("parseMembersCount skip: TotalSeats = %d, want 5", bd.TotalSeats)
	}
}
