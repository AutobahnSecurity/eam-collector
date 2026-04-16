package sender

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"time"

	"github.com/AutobahnSecurity/eam-collector/internal/parsers"
)

// Payload is the JSON body sent to POST /api/ingest
type Payload struct {
	DeviceID    string                    `json:"device_id"`
	UserEmail   string                    `json:"user_email,omitempty"`
	Records     []parsers.Record          `json:"records"`
	Identities  []parsers.AccountIdentity `json:"identities,omitempty"`
	BillingData []parsers.BillingData     `json:"billing_data,omitempty"`
}

// Response from the EAM ingest API
type Response struct {
	Stored  int `json:"stored"`
	Prompts int `json:"prompts"`
	Flagged int `json:"flagged"`
}

type Sender struct {
	url      string // ingest endpoint URL
	healthURL string // health check endpoint URL
	apiKey   string
	client   *http.Client
}

func New(baseURL, apiKey string) (*Sender, error) {
	ingestURL, err := url.JoinPath(baseURL, "/api/ingest")
	if err != nil {
		return nil, fmt.Errorf("invalid server URL %q: %w", baseURL, err)
	}
	healthURL, err := url.JoinPath(baseURL, "/api/health")
	if err != nil {
		return nil, fmt.Errorf("invalid server URL %q: %w", baseURL, err)
	}
	return &Sender{
		url:       ingestURL,
		healthURL: healthURL,
		apiKey:    apiKey,
		client:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

const batchSize = 500

// Send posts records to the EAM server, automatically splitting into batches.
func (s *Sender) Send(ctx context.Context, payload Payload) (*Response, error) {
	if len(payload.Records) == 0 {
		return &Response{}, nil
	}

	total := &Response{}

	for i := 0; i < len(payload.Records); i += batchSize {
		end := i + batchSize
		if end > len(payload.Records) {
			end = len(payload.Records)
		}

		batch := Payload{
			DeviceID:   payload.DeviceID,
			UserEmail:  payload.UserEmail,
			Records:    payload.Records[i:end],
			Identities: payload.Identities,
		}
		// Include billing data only in the first batch (per-device metadata, not per-record)
		if i == 0 {
			batch.BillingData = payload.BillingData
		}

		resp, err := s.sendBatch(ctx, batch)
		if err != nil {
			return total, fmt.Errorf("batch %d-%d: %w", i, end, err)
		}
		total.Stored += resp.Stored
		total.Prompts += resp.Prompts
		total.Flagged += resp.Flagged
	}

	return total, nil
}

func (s *Sender) sendBatch(ctx context.Context, payload Payload) (*Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter: 1s, 2s, 4s base + up to 50% jitter
			base := time.Duration(1<<uint(attempt)) * time.Second
			jitter := time.Duration(rand.Int63n(int64(base / 2)))
			wait := base + jitter
			log.Printf("[sender] Retry %d after %s", attempt, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", s.url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-EAM-Key", s.apiKey)

		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http request: %w", err)
			continue
		}

		// Cap response body read to 1MB to prevent OOM from malicious/buggy servers
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read response body: %w", err)
			continue
		}

		if resp.StatusCode == 401 {
			return nil, fmt.Errorf("authentication failed — check api_key in config")
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("server error %d: %s", resp.StatusCode, string(respBody))
			continue
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
		}

		var result Response
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("parse response: %w", err)
		}
		return &result, nil
	}

	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

// Ping checks if the EAM server is reachable.
// Tries GET /api/health first; falls back to a minimal POST /api/ingest
// if the health endpoint returns 404 (older server versions).
func (s *Sender) Ping() error {
	// Try dedicated health endpoint first
	req, err := http.NewRequest("GET", s.healthURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-EAM-Key", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("server unreachable: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == 401 {
		return fmt.Errorf("invalid API key")
	}
	// Health endpoint exists and responded
	if resp.StatusCode != 404 {
		return nil
	}

	// Fallback: older server without /api/health — use minimal ingest POST
	req, err = http.NewRequest("POST", s.url, bytes.NewReader([]byte(`{"device_id":"ping","records":[]}`)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EAM-Key", s.apiKey)

	resp, err = s.client.Do(req)
	if err != nil {
		return fmt.Errorf("server unreachable: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == 401 {
		return fmt.Errorf("invalid API key")
	}
	return nil
}
