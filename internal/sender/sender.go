package sender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/AutobahnSecurity/eam-collector/internal/parsers"
)

// Payload is the JSON body sent to POST /api/ingest
type Payload struct {
	DeviceID   string                    `json:"device_id"`
	UserEmail  string                    `json:"user_email,omitempty"`
	Records    []parsers.Record          `json:"records"`
	Identities []parsers.AccountIdentity `json:"identities,omitempty"`
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

func New(url, apiKey string) *Sender {
	return &Sender{
		url:    url + "/api/ingest",
		apiKey: apiKey,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

const batchSize = 500

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
			wait := time.Duration(attempt*2) * time.Second
			log.Printf("[sender] Retry %d after %s", attempt, wait)
			time.Sleep(wait)
		}

		req, err := http.NewRequest("POST", s.url, bytes.NewReader(body))
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

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

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
func (s *Sender) Ping() error {
	req, err := http.NewRequest("POST", s.url, bytes.NewReader([]byte(`{"device_id":"ping","records":[]}`)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EAM-Key", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("server unreachable: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == 401 {
		return fmt.Errorf("invalid API key")
	}
	return nil
}
