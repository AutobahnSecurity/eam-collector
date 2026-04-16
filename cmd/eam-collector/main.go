package main

import (
	"context"
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
	"github.com/AutobahnSecurity/eam-collector/internal/platform"
	"github.com/AutobahnSecurity/eam-collector/internal/sender"
	"github.com/AutobahnSecurity/eam-collector/internal/state"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Collector orchestrates the collection and sending of AI usage records.
type Collector struct {
	Parsers     []parsers.Parser
	Sender      *sender.Sender
	State       *state.Store
	DeviceID    string
	Identities  []parsers.AccountIdentity
	BillingData []parsers.BillingData
}

func main() {
	configPath := flag.String("config", "", "Path to config.yaml")
	stopFlag := flag.Bool("stop", false, "Stop the running collector")
	statusFlag := flag.Bool("status", false, "Check if the collector is running")
	flag.Parse()

	// Handle --stop and --status before anything else
	if *stopFlag || *statusFlag {
		home, err := platform.HomeDir()
		if err != nil {
			log.Fatalf("Cannot resolve home directory: %v", err)
		}
		lockPath := filepath.Join(home, ".eam-collector", "state.json.lock")
		pid := state.ReadPID(lockPath)

		if *statusFlag {
			if pid > 0 && processAlive(pid) {
				fmt.Printf("eam-collector is running (PID %d)\n", pid)
			} else {
				fmt.Println("eam-collector is not running")
			}
			return
		}

		// --stop
		if pid == 0 {
			fmt.Println("eam-collector is not running (no PID in lock file)")
			return
		}
		if !processAlive(pid) {
			fmt.Printf("eam-collector is not running (stale PID %d)\n", pid)
			return
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			log.Fatalf("Cannot find process %d: %v", pid, err)
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			log.Fatalf("Cannot stop collector (PID %d): %v", pid, err)
		}
		fmt.Printf("Sent SIGTERM to eam-collector (PID %d)\n", pid)
		return
	}

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
	s, err := sender.New(cfg.Server.URL, cfg.Server.APIKey)
	if err != nil {
		log.Fatalf("Failed to initialize sender: %v", err)
	}
	if err := s.Ping(); err != nil {
		log.Printf("[warn] Server ping failed: %v (will retry on first send)", err)
	} else {
		log.Println("[sender] Server reachable")
	}

	// Initialize state store
	home, err := platform.HomeDir()
	if err != nil {
		log.Fatalf("Cannot resolve home directory: %v", err)
	}
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

	// Graceful shutdown via context
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Println()
	log.Println("Collector running. Press Ctrl+C to stop.")
	fmt.Println()

	c := &Collector{
		Parsers:    activeParsers,
		Sender:     s,
		State:      stateStore,
		DeviceID:   deviceID,
		Identities: identities,
	}

	// Read billing data on startup (refreshed hourly)
	c.BillingData = parsers.ReadBillingData()
	lastBillingCheck := time.Now()

	// Main loop
	ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	defer ticker.Stop()

	// Run immediately on startup
	c.collect(ctx)

	for {
		select {
		case <-ticker.C:
			// Refresh billing data hourly
			if time.Since(lastBillingCheck) > time.Hour {
				c.BillingData = parsers.ReadBillingData()
				lastBillingCheck = time.Now()
			}
			c.collect(ctx)
		case <-ctx.Done():
			log.Println("Shutting down")
			_ = stateStore.Save()
			stateStore.Close()
			return
		}
	}
}

func (c *Collector) collect(ctx context.Context) {
	var allRecords []parsers.Record

	for _, p := range c.Parsers {
		prevState := c.State.Get(p.Name())
		records, newState, err := p.Collect(prevState)
		if err != nil {
			log.Printf("[%s] Error: %v", p.Name(), err)
			continue
		}
		if len(records) > 0 {
			log.Printf("[%s] Collected %d records", p.Name(), len(records))
			allRecords = append(allRecords, records...)
		}
		c.State.Set(p.Name(), newState)
	}

	if len(allRecords) == 0 {
		return
	}

	payload := sender.Payload{
		DeviceID:    c.DeviceID,
		Records:     allRecords,
		Identities:  c.Identities,
		BillingData: c.BillingData,
	}

	resp, err := c.Sender.Send(ctx, payload)
	if err != nil {
		log.Printf("[sender] Failed: %v", err)
		return
	}

	log.Printf("[sender] Stored %d usage records, %d prompts (%d flagged)",
		resp.Stored, resp.Prompts, resp.Flagged)

	// Save state only after successful send
	if err := c.State.Save(); err != nil {
		log.Printf("[state] Failed to save: %v", err)
	}
}

func createParser(name string) parsers.Parser {
	switch name {
	case "claude", "claude_code":
		return parsers.NewClaudeParser()
	case "cursor":
		return parsers.NewCursorParser()
	case "copilot":
		return parsers.NewCopilotParser()
	case "continuedev":
		return parsers.NewContinueParser()
	default:
		return nil
	}
}

func findConfig() string {
	home, err := platform.HomeDir()
	if err != nil {
		home = ""
	}
	candidates := []string{
		filepath.Join(home, ".eam-collector", "config.yaml"),
		"/etc/eam-collector/config.yaml",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// processAlive checks whether a process with the given PID is still running.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, Signal(0) checks if process exists without actually signaling it
	return proc.Signal(syscall.Signal(0)) == nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	h = strings.ToLower(h)
	h = strings.TrimSuffix(h, ".local")
	return h
}
