package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AutobahnSecurity/eam-collector/internal/config"
	"github.com/AutobahnSecurity/eam-collector/internal/parsers"
	"github.com/AutobahnSecurity/eam-collector/internal/sender"
	"github.com/AutobahnSecurity/eam-collector/internal/state"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Set via ldflags at build time
var (
	version = "dev"
	build   = "unknown"
)

func main() {
	configPath := flag.String("config", "", "Path to config.yaml")
	showVersion := flag.Bool("version", false, "Print version and exit")
	runSetup := flag.Bool("setup", false, "Interactive setup wizard")
	flag.Parse()

	if *showVersion {
		fmt.Printf("eam-collector %s (build %s)\n", version, build)
		os.Exit(0)
	}

	if *runSetup {
		setupWizard()
		os.Exit(0)
	}

	// Find config file
	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = findConfig()
	}
	if cfgPath == "" {
		// Auto-run setup if stdin is a terminal
		if isTerminal() {
			fmt.Println("No config file found. Running setup wizard...")
			fmt.Println()
			setupWizard()
			// Reload config after setup
			cfgPath = findConfig()
			if cfgPath == "" {
				log.Fatal("Setup did not create a config file")
			}
		} else {
			log.Fatal("No config file found. Run: eam-collector --setup")
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	fmt.Println("══════════════════════════════════════════")
	fmt.Println("  EAM Collector — AI Usage Agent")
	fmt.Println("══════════════════════════════════════════")
	log.Printf("[config] Server: %s", cfg.Server.URL)
	log.Printf("[config] Interval: %ds", cfg.Interval)

	// Initialize sender
	s := sender.New(cfg.Server.URL, cfg.Server.APIKey)
	if err := s.Ping(); err != nil {
		log.Printf("[warn] Server ping failed: %v (will retry on first send)", err)
	} else {
		log.Println("[sender] Server reachable")
	}

	// Initialize state store
	home, _ := os.UserHomeDir()
	stateStore := state.New(filepath.Join(home, ".eam-collector"))
	if err := stateStore.Load(); err != nil {
		if strings.Contains(err.Error(), "already running") {
			log.Fatalf("[error] %v — kill the other process first: pkill -f eam-collector", err)
		}
		log.Printf("[warn] Failed to load state: %v (starting fresh)", err)
	}

	// Initialize parsers
	var activeParsers []parsers.Parser
	for name, pcfg := range cfg.Parsers {
		if !pcfg.Enabled {
			log.Printf("[%s] Disabled", name)
			continue
		}
		p := createParser(name)
		if p == nil {
			log.Printf("[%s] Unknown parser, skipping", name)
			continue
		}
		p.SetLookback(cfg.Lookback)
		activeParsers = append(activeParsers, p)
		log.Printf("[%s] Enabled", name)
	}

	if len(activeParsers) == 0 {
		log.Fatal("No parsers enabled. Enable at least one in config.yaml")
	}

	// Resolve device ID — user email comes from MDE on the server side
	deviceID := hostname()
	log.Printf("[device] %s", deviceID)

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println()
	log.Println("Collector running. Press Ctrl+C to stop.")
	fmt.Println()

	// Main loop
	ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	defer ticker.Stop()

	// Run immediately on startup
	collect(activeParsers, s, stateStore, deviceID)

	for {
		select {
		case <-ticker.C:
			collect(activeParsers, s, stateStore, deviceID)
		case sig := <-sigCh:
			log.Printf("Received %s, shutting down", sig)
			_ = stateStore.Save()
			stateStore.Close()
			return
		}
	}
}

func collect(pp []parsers.Parser, s *sender.Sender, store *state.Store, deviceID string) {
	var allRecords []parsers.Record
	var healths []parsers.Health
	var parserSummary []string

	for _, p := range pp {
		prevState := store.Get(p.Name())
		records, newState, err := p.Collect(prevState)

		h := parsers.Health{
			Parser:   p.Name(),
			DataPath: p.DataDir(),
			Records:  len(records),
		}

		if err != nil {
			h.Status = "error"
			h.Error = err.Error()
			log.Printf("[%s] Error: %v", p.Name(), err)
			parserSummary = append(parserSummary, fmt.Sprintf("%s:err", p.Name()))
		} else if _, statErr := os.Stat(p.DataDir()); os.IsNotExist(statErr) {
			h.Status = "not_installed"
		} else if len(records) == 0 {
			h.Status = "ok"
		} else {
			h.Status = "ok"
			log.Printf("[%s] Collected %d records", p.Name(), len(records))
			allRecords = append(allRecords, records...)
			parserSummary = append(parserSummary, fmt.Sprintf("%s:%d", p.Name(), len(records)))
		}

		healths = append(healths, h)
		if err == nil {
			store.Set(p.Name(), newState)
		}
	}

	// Send heartbeat to server so it knows we're alive, even with no data
	if len(allRecords) == 0 {
		if err := s.Heartbeat(deviceID); err != nil {
			log.Printf("[heartbeat] Failed: %v", err)
		} else {
			log.Println("[heartbeat] OK")
		}
		// Save state even with no records — parser offsets still need persisting
		if err := store.Save(); err != nil {
			log.Printf("[state] Failed to save: %v", err)
		}
		return
	}

	// Log health warnings
	for _, h := range healths {
		if h.Status == "error" || h.Status == "degraded" {
			log.Printf("[health] %s: %s — %s", h.Parser, h.Status, h.Error)
		}
	}

	if len(allRecords) == 0 {
		return
	}

	// Per-session identity: snapshot the current account when a session is
	// first seen. This ensures governance is determined by the account active
	// at session creation, not the current account (which may have changed).
	//
	// Each identity's Tool field matches the record Source it governs
	// (e.g., "claude-code" identity governs "claude-code" records).
	currentIdentities := parsers.ReadClaudeIdentities()
	identityBySource := make(map[string]parsers.AccountIdentity, len(currentIdentities))
	for _, id := range currentIdentities {
		identityBySource[id.Tool] = id
	}
	sessionIDs := loadSessionIdentities(store)

	for _, r := range allRecords {
		entry, exists := sessionIDs[r.SessionID]
		if !exists {
			// Prefer per-record identity (from parser, e.g. Desktop directory path),
			// fall back to global identity lookup (statsig cache).
			// This handles users who only use Desktop without ever using the CLI.
			var id *parsers.AccountIdentity
			if r.Identity != nil {
				id = r.Identity
			} else if globalID, ok := identityBySource[r.Source]; ok {
				id = &globalID
			}
			if id != nil {
				sessionIDs[r.SessionID] = sessionIdentityEntry{
					Identity: *id,
					LastSeen: time.Now(),
				}
			}
		} else {
			// Refresh last-seen so active sessions don't expire
			entry.LastSeen = time.Now()
			sessionIDs[r.SessionID] = entry
		}
	}

	// Expire identities not seen for 7 days (handles paused sessions that resume)
	const identityTTL = 7 * 24 * time.Hour
	for sid, entry := range sessionIDs {
		if time.Since(entry.LastSeen) > identityTTL {
			delete(sessionIDs, sid)
		}
	}
	saveSessionIdentities(store, sessionIDs)

	// Group records by org UUID so each batch carries the correct identity.
	// Accumulate all unique tool identities per org (e.g., both claude-code
	// and claude-desktop when both sources have records for the same org).
	type batch struct {
		identity []parsers.AccountIdentity
		records  []parsers.Record
		tools    map[string]bool // track which tools already added
	}
	groups := make(map[string]*batch)
	for _, r := range allRecords {
		orgKey := ""
		var id *parsers.AccountIdentity
		if entry, ok := sessionIDs[r.SessionID]; ok {
			orgKey = entry.Identity.OrganizationUUID
			id = &entry.Identity
		}
		b := groups[orgKey]
		if b == nil {
			b = &batch{tools: make(map[string]bool)}
			groups[orgKey] = b
		}
		if id != nil && !b.tools[id.Tool] {
			b.identity = append(b.identity, *id)
			b.tools[id.Tool] = true
		}
		b.records = append(b.records, r)
	}

	// Send separate payloads per identity group
	allOK := true
	for _, b := range groups {
		for _, id := range b.identity {
			log.Printf("[identity] %s account=%s org=%s (%d records)",
				id.Tool, id.AccountUUID, id.OrganizationUUID, len(b.records))
		}
		payload := sender.Payload{
			DeviceID:   deviceID,
			Records:    b.records,
			Identities: b.identity,
			Healths:    healths,
		}
		resp, err := s.Send(payload)
		if err != nil {
			log.Printf("[sender] Failed: %v", err)
			allOK = false
			continue
		}
		log.Printf("[sender] Stored %d usage records, %d prompts (%d flagged)",
			resp.Stored, resp.Prompts, resp.Flagged)
	}

	// Save state only after all sends succeed
	if allOK {
		if err := store.Save(); err != nil {
			log.Printf("[state] Failed to save: %v", err)
		}
	}
}

type sessionIdentityEntry struct {
	Identity parsers.AccountIdentity
	LastSeen time.Time
}

// loadSessionIdentities restores the session→identity map from state.
func loadSessionIdentities(store *state.Store) map[string]sessionIdentityEntry {
	result := make(map[string]sessionIdentityEntry)
	raw, ok := store.Get("_identities")["sessions"]
	if !ok {
		return result
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return result
	}
	for sid, v := range m {
		idMap, ok := v.(map[string]any)
		if !ok {
			continue
		}
		acct, _ := idMap["account_uuid"].(string)
		org, _ := idMap["organization_uuid"].(string)
		tool, _ := idMap["tool"].(string)
		lastSeenStr, _ := idMap["last_seen"].(string)
		if acct == "" {
			continue
		}
		lastSeen, err := time.Parse(time.RFC3339, lastSeenStr)
		if err != nil {
			lastSeen = time.Now() // treat unparseable as fresh
		}
		result[sid] = sessionIdentityEntry{
			Identity: parsers.AccountIdentity{
				AccountUUID:      acct,
				OrganizationUUID: org,
				Tool:             tool,
			},
			LastSeen: lastSeen,
		}
	}
	return result
}

// saveSessionIdentities persists the session→identity map to state.
func saveSessionIdentities(store *state.Store, entries map[string]sessionIdentityEntry) {
	m := make(map[string]any, len(entries))
	for sid, entry := range entries {
		m[sid] = map[string]any{
			"account_uuid":      entry.Identity.AccountUUID,
			"organization_uuid": entry.Identity.OrganizationUUID,
			"tool":              entry.Identity.Tool,
			"last_seen":         entry.LastSeen.Format(time.RFC3339),
		}
	}
	store.Set("_identities", map[string]any{"sessions": m})
}

func createParser(name string) parsers.Parser {
	switch name {
	case "claude", "claude_code", "claude_desktop":
		return parsers.NewClaudeParser()
	default:
		return nil
	}
}

func findConfig() string {
	candidates := []string{
		filepath.Join(homeDir(), ".eam-collector", "config.yaml"),
		"/etc/eam-collector/config.yaml",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// hostnameSuffixes must match normalizeHostname() in classification.ts on the server.
var hostnameSuffixes = []string{".local", ".lan", ".home", ".internal"}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	h = strings.ToLower(h)
	for _, suffix := range hostnameSuffixes {
		h = strings.TrimSuffix(h, suffix)
	}
	return h
}

func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func setupWizard() {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println("══════════════════════════════════════════")
	fmt.Println("  EAM Collector Setup")
	fmt.Println("══════════════════════════════════════════")
	fmt.Println()

	// Server URL
	fmt.Print("Server URL [https://eam.devops.atbhn.io]: ")
	scanner.Scan()
	serverURL := strings.TrimSpace(scanner.Text())
	if serverURL == "" {
		serverURL = "https://eam.devops.atbhn.io"
	}

	// API Key
	fmt.Print("API Key (ek_...): ")
	scanner.Scan()
	apiKey := strings.TrimSpace(scanner.Text())
	if apiKey == "" || !strings.HasPrefix(apiKey, "ek_") {
		log.Fatal("API key is required and must start with ek_")
	}

	// Test connection
	fmt.Print("Testing connection... ")
	s := sender.New(serverURL, apiKey)
	if err := s.Ping(); err != nil {
		fmt.Println("FAILED")
		log.Fatalf("Cannot reach server: %v", err)
	}
	fmt.Println("OK")

	// Write config
	configDir := filepath.Join(homeDir(), ".eam-collector")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		log.Fatalf("Cannot create config directory: %v", err)
	}

	configContent := fmt.Sprintf(`server:
  url: %s
  api_key: %s

interval: 60
lookback: 24

parsers:
  claude:
    enabled: true
`, serverURL, apiKey)

	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		log.Fatalf("Cannot write config: %v", err)
	}

	fmt.Println()
	fmt.Printf("Config saved to %s\n", configPath)
	fmt.Println()
	fmt.Println("Start the collector:")
	fmt.Println("  brew services start eam-collector")
	fmt.Println()
}

