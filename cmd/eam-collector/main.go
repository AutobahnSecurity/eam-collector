package main

import (
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

func main() {
	configPath := flag.String("config", "", "Path to config.yaml")
	flag.Parse()

	// Find config file
	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = findConfig()
	}
	if cfgPath == "" {
		log.Fatal("No config file found. Use -config flag or create ~/.eam-collector/config.yaml")
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

	// Detect AI account identities (from statsig cache + Desktop session paths)
	identities := parsers.ReadClaudeIdentities()
	if len(identities) > 0 {
		for _, id := range identities {
			log.Printf("[identity] %s account=%s org=%s", id.Tool, id.AccountUUID, id.OrganizationUUID)
		}
	} else {
		log.Println("[identity] No Claude accounts detected")
	}

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println()
	log.Println("Collector running. Press Ctrl+C to stop.")
	fmt.Println()

	// Main loop
	ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	defer ticker.Stop()

	// Read billing data on startup (refreshed hourly)
	billingData := parsers.ReadBillingData()
	lastBillingCheck := time.Now()

	// Run immediately on startup
	collect(activeParsers, s, stateStore, deviceID, identities, billingData)

	for {
		select {
		case <-ticker.C:
			// Refresh billing data hourly
			if time.Since(lastBillingCheck) > time.Hour {
				billingData = parsers.ReadBillingData()
				lastBillingCheck = time.Now()
			}
			collect(activeParsers, s, stateStore, deviceID, identities, billingData)
		case sig := <-sigCh:
			log.Printf("Received %s, shutting down", sig)
			_ = stateStore.Save()
			stateStore.Close()
			return
		}
	}
}

func collect(pp []parsers.Parser, s *sender.Sender, store *state.Store, deviceID string, identities []parsers.AccountIdentity, billingData []parsers.BillingData) {
	var allRecords []parsers.Record

	for _, p := range pp {
		prevState := store.Get(p.Name())
		records, newState, err := p.Collect(prevState)
		if err != nil {
			log.Printf("[%s] Error: %v", p.Name(), err)
			continue
		}
		if len(records) > 0 {
			log.Printf("[%s] Collected %d records", p.Name(), len(records))
			allRecords = append(allRecords, records...)
		}
		store.Set(p.Name(), newState)
	}

	if len(allRecords) == 0 {
		return
	}

	// Send to EAM server — no user_email, resolved from MDE on server side
	ids := make([]parsers.AccountIdentity, len(identities))
	copy(ids, identities)
	payload := sender.Payload{
		DeviceID:    deviceID,
		Records:     allRecords,
		Identities:  ids,
		BillingData: billingData,
	}

	resp, err := s.Send(payload)
	if err != nil {
		log.Printf("[sender] Failed: %v", err)
		return
	}

	log.Printf("[sender] Stored %d usage records, %d prompts (%d flagged)",
		resp.Stored, resp.Prompts, resp.Flagged)

	// Save state only after successful send
	if err := store.Save(); err != nil {
		log.Printf("[state] Failed to save: %v", err)
	}
}

func createParser(name string) parsers.Parser {
	switch name {
	case "claude", "claude_code":
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

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	// Normalize: lowercase, strip .local suffix to match MDE device names
	h = strings.ToLower(h)
	h = strings.TrimSuffix(h, ".local")
	return h
}

