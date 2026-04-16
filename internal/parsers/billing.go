package parsers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AutobahnSecurity/eam-collector/internal/platform"
	"github.com/klauspost/compress/zstd"
)

// billingLDBSearchWindow is the byte window around an org UUID to search for billing_type.
// This is a heuristic based on the observed format of Claude Desktop's Local Storage.
const billingLDBSearchWindow = 500

// BillingData holds subscription/billing info extracted from Claude Desktop's local cache.
type BillingData struct {
	OrganizationUUID string          `json:"organization_uuid"`
	BillingType      string          `json:"billing_type,omitempty"`
	Plan             string          `json:"plan,omitempty"`
	SeatTiers        []SeatTierCount `json:"seat_tiers,omitempty"`
	TotalSeats       int             `json:"total_seats,omitempty"`
	CollectedAt      string          `json:"collected_at"`
}

// SeatTierCount holds a seat tier name and its count.
type SeatTierCount struct {
	Tier  string `json:"tier"`
	Count int    `json:"count"`
}

var (
	membersLimitRe = regexp.MustCompile(`/api/organizations/([0-9a-f-]{36})/members_limit`)
	membersCountRe = regexp.MustCompile(`/api/organizations/([0-9a-f-]{36})/members/counts`)
	billingTypeRe  = regexp.MustCompile(`"billing_type"\s*:\s*"([^"]+)"`)
)

// ReadBillingData extracts subscription/billing info from Claude Desktop's HTTP cache.
// Reads zstd-compressed API responses cached by the Electron app.
func ReadBillingData() []BillingData {
	home, err := platform.HomeDir()
	if err != nil {
		return nil
	}
	cacheDir := platform.ClaudeDesktopCacheDir(home)
	if cacheDir == "" {
		return nil
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil
	}

	// Collect billing data per org UUID
	orgData := make(map[string]*BillingData)

	for _, entry := range entries {
		name := entry.Name()
		// Chrome cache large entries are *_0 files
		if entry.IsDir() || !strings.HasSuffix(name, "_0") {
			continue
		}

		path := filepath.Join(cacheDir, name)
		info, err := entry.Info()
		if err != nil || info.Size() > 1024*1024 || info.Size() < 50 {
			continue // skip very large or very small files
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		text := string(data)

		// Check if this is a members_limit or members/counts response
		var orgUUID string
		var isLimit, isCount bool

		if m := membersLimitRe.FindStringSubmatch(text); m != nil {
			orgUUID = m[1]
			isLimit = true
		} else if m := membersCountRe.FindStringSubmatch(text); m != nil {
			orgUUID = m[1]
			isCount = true
		} else {
			// Not a billing endpoint
			continue
		}

		// Try to decompress the response body (zstd)
		body := decompressZstdBody(data)
		if body == "" {
			body = text // fallback to raw
		}

		bd, ok := orgData[orgUUID]
		if !ok {
			bd = &BillingData{
				OrganizationUUID: orgUUID,
				CollectedAt:      time.Now().UTC().Format(time.RFC3339),
			}
			orgData[orgUUID] = bd
		}

		if isLimit {
			parseMembersLimit(body, bd)
		}
		if isCount {
			parseMembersCount(body, bd)
		}
	}

	// Also read billing_type from Local Storage LevelDB
	readBillingTypeFromLDB(orgData)

	var results []BillingData
	for _, bd := range orgData {
		if bd.TotalSeats > 0 || bd.BillingType != "" {
			results = append(results, *bd)
		}
	}

	if len(results) > 0 {
		for _, r := range results {
			tiers := make([]string, 0, len(r.SeatTiers))
			for _, t := range r.SeatTiers {
				tiers = append(tiers, fmt.Sprintf("%s=%d", t.Tier, t.Count))
			}
			log.Printf("[billing] org=%s plan=%s billing=%s seats=%d tiers=[%s]",
				r.OrganizationUUID[:8], r.Plan, r.BillingType, r.TotalSeats, strings.Join(tiers, ", "))
		}
	}

	return results
}

// zstdDecoder is a lazily initialized, reusable zstd decoder.
// Using sync.Once instead of sync.Pool to avoid the stale-state pitfall:
// sync.Pool items can hold references to previously decoded buffers,
// and zstd.Decoder.Reset(nil) is needed between uses to release them.
// A single shared decoder with a mutex is simpler and correct for our
// use case (billing data decoded once per hour, not a hot path).
var (
	zstdDecoder     *zstd.Decoder
	zstdDecoderOnce sync.Once
	zstdDecoderMu   sync.Mutex
)

func getZstdDecoder() *zstd.Decoder {
	zstdDecoderOnce.Do(func() {
		dec, err := zstd.NewReader(nil)
		if err != nil {
			log.Printf("[billing] Failed to create zstd decoder: %v", err)
			return
		}
		zstdDecoder = dec
	})
	return zstdDecoder
}

// decompressZstdBody finds and decompresses zstd content in a cache entry.
func decompressZstdBody(data []byte) string {
	// Find zstd magic bytes: 0x28 0xB5 0x2F 0xFD
	idx := bytes.Index(data, []byte{0x28, 0xB5, 0x2F, 0xFD})
	if idx == -1 {
		return ""
	}

	dec := getZstdDecoder()
	if dec == nil {
		return ""
	}

	zstdDecoderMu.Lock()
	defer zstdDecoderMu.Unlock()

	// DecodeAll may return both decoded data AND an error when there's
	// trailing garbage after the zstd frame. Accept the result if we got data.
	decoded, err := dec.DecodeAll(data[idx:], nil)
	if err != nil && len(decoded) == 0 {
		log.Printf("[billing] zstd decompression failed: %v", err)
		return ""
	}
	if len(decoded) > 0 {
		return string(decoded)
	}
	return ""
}

// parseMembersLimit extracts seat_tier_quantities from a /members_limit response.
func parseMembersLimit(body string, bd *BillingData) {
	// {"seat_tier_quantities": {"team_standard": 21, "team_tier_1": 2}, "minimum_seats": 5, ...}
	var resp struct {
		SeatTierQuantities map[string]int `json:"seat_tier_quantities"`
		MinimumSeats       int            `json:"minimum_seats"`
		MembersLimit       int            `json:"members_limit"`
	}

	// Find the JSON body
	start := strings.Index(body, `{"seat_tier_quantities"`)
	if start == -1 {
		start = strings.Index(body, `{"members_limit"`)
	}
	if start == -1 {
		return
	}

	// Find matching closing brace
	jsonStr := extractJSON(body, start)
	if jsonStr == "" {
		return
	}

	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return
	}

	var tiers []SeatTierCount
	total := 0
	for tier, count := range resp.SeatTierQuantities {
		if count > 0 {
			tiers = append(tiers, SeatTierCount{Tier: tier, Count: count})
			total += count
		}
	}
	bd.SeatTiers = tiers
	bd.TotalSeats = total
}

// parseMembersCount extracts by_seat_tier from a /members/counts response.
func parseMembersCount(body string, bd *BillingData) {
	var resp struct {
		Total      int            `json:"total"`
		BySeatTier map[string]int `json:"by_seat_tier"`
	}

	start := strings.Index(body, `{"total"`)
	if start == -1 {
		return
	}

	jsonStr := extractJSON(body, start)
	if jsonStr == "" {
		return
	}

	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return
	}

	// Use counts data if we don't have seat_tier_quantities yet
	if len(bd.SeatTiers) == 0 && len(resp.BySeatTier) > 0 {
		var tiers []SeatTierCount
		total := 0
		for tier, count := range resp.BySeatTier {
			if tier != "unassigned" && count > 0 {
				tiers = append(tiers, SeatTierCount{Tier: tier, Count: count})
				total += count
			}
		}
		bd.SeatTiers = tiers
		bd.TotalSeats = total
	}
}

// readBillingTypeFromLDB reads billing_type and plan from Local Storage LevelDB.
func readBillingTypeFromLDB(orgData map[string]*BillingData) {
	home, err := platform.HomeDir()
	if err != nil {
		return
	}
	ldbDir := platform.ClaudeDesktopLDBDir(home)
	if ldbDir == "" {
		return
	}

	entries, err := os.ReadDir(ldbDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".ldb") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(ldbDir, entry.Name()))
		if err != nil {
			continue
		}

		text := string(data)

		// Find org UUIDs and their associated billing_type
		for orgUUID := range orgData {
			idx := strings.Index(text, orgUUID)
			if idx == -1 {
				continue
			}

			// Search in a window around the org UUID
			start := idx
			end := idx + billingLDBSearchWindow
			if end > len(text) {
				end = len(text)
			}
			window := text[start:end]

			if m := billingTypeRe.FindStringSubmatch(window); m != nil {
				orgData[orgUUID].BillingType = m[1]
			}

			// Look for plan type. "raven" is the internal codename for the
			// Claude Team/Enterprise plan (distinguished by "commercial_use" billing).
			if strings.Contains(window, "commercial_use") {
				orgData[orgUUID].Plan = "raven"
			} else if strings.Contains(window, `"pro"`) || strings.Contains(window, "_pro") {
				orgData[orgUUID].Plan = "claude_pro"
			}
		}
	}
}

// extractJSON extracts a JSON object starting at the given position.
// Uses json.Decoder to correctly handle braces inside string values.
func extractJSON(s string, start int) string {
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return ""
	}
	return string(raw)
}

