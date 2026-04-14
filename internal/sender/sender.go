package sender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AutobahnSecurity/eam-collector/internal/parsers"
)

// Payload is the JSON body sent to POST /api/ingest
type Payload struct {
	DeviceID   string                    `json:"device_id"`
	UserEmail  string                    `json:"user_email,omitempty"`
	Records    []parsers.Record          `json:"records"`
	Identities []parsers.AccountIdentity `json:"identities,omitempty"`
	Healths    []parsers.Health          `json:"healths,omitempty"`
}

// Response from the EAM ingest API
type Response struct {
	Stored  int `json:"stored"`
	Prompts int `json:"prompts"`
	Flagged int `json:"flagged"`
}

type Sender struct {
	url    string
	apiKey string
	client *http.Client
}

// batchSize is the maximum number of records per HTTP request.
const batchSize = 500

// New creates a Sender for the given EAM server URL and API key.
// Returns an error if the URL is invalid or uses an insecure scheme.
func New(baseURL, apiKey string) (*Sender, error) {
	baseURL = strings.TrimRight(baseURL, "/")

	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL %q: %w", baseURL, err)
	}

	// Enforce HTTPS unless targeting localhost/loopback for development
	if u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		return nil, fmt.Errorf("server URL must use HTTPS (got %s); API key would be sent in plaintext", u.Scheme)
	}

	return &Sender{
		url:    baseURL + "/api/ingest",
		apiKey: apiKey,
		client: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Send posts records to the EAM server, automatically splitting into batches.
func (s *Sender) Send(payload Payload) (*Response, error) {
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

		resp, err := s.sendBatch(batch)
		if err != nil {
			return total, fmt.Errorf("batch %d-%d: %w", i, end, err)
		}
		total.Stored += resp.Stored
		total.Prompts += resp.Prompts
		total.Flagged += resp.Flagged
	}

	return total, nil
}

func (s *Sender) sendBatch(payload Payload) (*Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter: ~2s, ~4s
			base := time.Duration(1<<uint(attempt)) * time.Second
			jitter := time.Duration(rand.Int63n(int64(base / 2)))
			wait := base + jitter
			log.Printf("[sender] Retry %d after %s", attempt, wait)
			time.Sleep(wait)
		}

		resp, retryable, err := s.doRequest(body)
		if err != nil {
			if !retryable {
				return nil, err // terminal error, don't retry
			}
			lastErr = err
			continue
		}
		return resp, nil
	}

	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

// maxResponseSize caps response body reads to prevent memory exhaustion.
const maxResponseSize = 1024 * 1024 // 1 MB

// doRequest sends a single POST request and processes the response.
// Returns (result, retryable, error). Network errors and 5xx are retryable;
// 4xx and parse errors are terminal.
func (s *Sender) doRequest(body []byte) (*Response, bool, error) {
	req, err := http.NewRequest("POST", s.url, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EAM-Key", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("http request: %w", err) // retryable
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	resp.Body.Close()
	if err != nil {
		return nil, true, fmt.Errorf("read response: %w", err) // retryable
	}

	if resp.StatusCode == 401 {
		return nil, false, fmt.Errorf("authentication failed — check api_key in config")
	}
	if resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("server error %d: %s", resp.StatusCode, string(respBody)) // retryable
	}
	if resp.StatusCode != 200 {
		return nil, false, fmt.Errorf("client error %d: %s", resp.StatusCode, string(respBody)) // terminal
	}

	var result Response
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, false, fmt.Errorf("parse response: %w", err)
	}
	return &result, false, nil
}

// Ping checks if the EAM server is reachable and the API key is valid.
func (s *Sender) Ping() error {
	body, _ := json.Marshal(map[string]any{
		"device_id": "ping",
		"records":   []any{},
		"heartbeat": true,
	})

	req, err := http.NewRequest("POST", s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EAM-Key", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("server unreachable: %w", err)
	}
	io.Copy(io.Discard, resp.Body) // drain for connection reuse
	resp.Body.Close()

	if resp.StatusCode == 401 {
		return fmt.Errorf("invalid API key")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return nil
}

// Heartbeat sends a lightweight ping to the server so it knows the collector is alive.
// Called when no new records are available.
func (s *Sender) Heartbeat(deviceID string) error {
	body, _ := json.Marshal(map[string]any{
		"device_id": deviceID,
		"records":   []any{},
		"heartbeat": true,
	})

	req, err := http.NewRequest("POST", s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EAM-Key", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body) // drain for connection reuse
	resp.Body.Close()

	if resp.StatusCode == 401 {
		return fmt.Errorf("invalid API key")
	}
	return nil
}
