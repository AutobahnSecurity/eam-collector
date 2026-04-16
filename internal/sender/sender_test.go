package sender

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AutobahnSecurity/eam-collector/internal/parsers"
)

func TestBatchSplitting(t *testing.T) {
	var mu sync.Mutex
	var received []Payload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode request body: %v", err)
			http.Error(w, "bad request", 400)
			return
		}
		mu.Lock()
		received = append(received, p)
		mu.Unlock()

		resp := Response{Stored: len(p.Records), Prompts: 0, Flagged: 0}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	s, err := New(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Build 501 records
	records := make([]parsers.Record, 501)
	for i := range records {
		records[i] = parsers.Record{
			Source:    "test",
			SessionID: fmt.Sprintf("sess-%d", i),
			Timestamp: "2025-01-01T00:00:00Z",
			Role:      "user",
			Content:   fmt.Sprintf("message %d", i),
			AIVendor:  "test-vendor",
		}
	}

	billing := []parsers.BillingData{
		{
			OrganizationUUID: "org-uuid-1234",
			BillingType:      "enterprise",
			TotalSeats:       10,
			CollectedAt:      "2025-01-01T00:00:00Z",
		},
	}

	payload := Payload{
		DeviceID:    "dev-1",
		Records:     records,
		BillingData: billing,
	}

	resp, err := s.Send(context.Background(), payload)
	if err != nil {
		t.Fatalf("Send() error: %v", err)
	}

	t.Run("two batches sent", func(t *testing.T) {
		if len(received) != 2 {
			t.Fatalf("expected 2 batches, got %d", len(received))
		}
	})

	t.Run("first batch has 500 records", func(t *testing.T) {
		if len(received[0].Records) != 500 {
			t.Errorf("first batch: expected 500 records, got %d", len(received[0].Records))
		}
	})

	t.Run("second batch has 1 record", func(t *testing.T) {
		if len(received[1].Records) != 1 {
			t.Errorf("second batch: expected 1 record, got %d", len(received[1].Records))
		}
	})

	t.Run("first batch includes billing data", func(t *testing.T) {
		if len(received[0].BillingData) != 1 {
			t.Errorf("first batch: expected 1 billing entry, got %d", len(received[0].BillingData))
		}
	})

	t.Run("second batch omits billing data", func(t *testing.T) {
		if len(received[1].BillingData) != 0 {
			t.Errorf("second batch: expected 0 billing entries, got %d", len(received[1].BillingData))
		}
	})

	t.Run("all records accounted for", func(t *testing.T) {
		if resp.Stored != 501 {
			t.Errorf("expected 501 stored, got %d", resp.Stored)
		}
		totalReceived := len(received[0].Records) + len(received[1].Records)
		if totalReceived != 501 {
			t.Errorf("total records across batches: expected 501, got %d", totalReceived)
		}
	})
}

func TestEmptyPayload(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s, err := New(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	resp, err := s.Send(context.Background(), Payload{DeviceID: "dev-1", Records: nil})
	if err != nil {
		t.Fatalf("Send() error: %v", err)
	}

	t.Run("no HTTP calls made", func(t *testing.T) {
		if called {
			t.Error("expected no HTTP calls for empty payload")
		}
	})

	t.Run("returns empty response", func(t *testing.T) {
		if resp.Stored != 0 || resp.Prompts != 0 || resp.Flagged != 0 {
			t.Errorf("expected zero response, got stored=%d prompts=%d flagged=%d",
				resp.Stored, resp.Prompts, resp.Flagged)
		}
	})
}

func TestAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	s, err := New(srv.URL, "bad-key")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	payload := Payload{
		DeviceID: "dev-1",
		Records: []parsers.Record{
			{
				Source:    "test",
				SessionID: "sess-1",
				Timestamp: "2025-01-01T00:00:00Z",
				Role:      "user",
				Content:   "hello",
				AIVendor:  "test-vendor",
			},
		},
	}

	_, err = s.Send(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}

	t.Run("error mentions authentication", func(t *testing.T) {
		if got := err.Error(); !strings.Contains(got, "authentication failed") {
			t.Errorf("expected error containing 'authentication failed', got: %s", got)
		}
	})
}
