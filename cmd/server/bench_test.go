package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	kb        = 1024
	chunkSize = 100
)

// generateLargeString returns a string of approximately sizeKB kilobytes.
func generateLargeString(sizeKB int) string {
	n := sizeKB * kb
	if n <= 0 {
		return ""
	}
	return strings.Repeat("X", n)
}

// generateLargePayload splits the string into 100-byte chunks and assigns them
// to keys key_0, key_1, ...
func generateLargePayload(sizeKB int) map[string]string {
	s := generateLargeString(sizeKB)
	m := make(map[string]string, (len(s)+chunkSize-1)/chunkSize)
	for i := 0; i < len(s); i += chunkSize {
		key := fmt.Sprintf("key_%d", i/chunkSize)
		end := i + chunkSize
		if end > len(s) {
			end = len(s)
		}
		m[key] = s[i:end]
	}
	return m
}

// makeRunsBody builds a JSON array body for POST /runs with the given batch size and
// sizeKB for inputs/outputs/metadata payloads.
func makeRunsBody(batch, sizeKB int) []byte {
	runs := make([]map[string]any, 0, batch)
	for i := 0; i < batch; i++ {
		runs = append(runs, map[string]any{
			"trace_id": uuid.New().String(),
			"name":     fmt.Sprintf("Benchmark Run %s", uuid.New().String()),
			"inputs":   generateLargePayload(sizeKB),
			"outputs":  generateLargePayload(sizeKB),
			"metadata": generateLargePayload(sizeKB),
		})
	}
	b, _ := json.Marshal(runs)
	return b
}

func BenchmarkCreateRuns(b *testing.B) {
	r, _ := newTestRouter(b)
	ts := httptest.NewServer(r)
	defer ts.Close()

	cases := []struct {
		name      string
		batch     int
		fieldSize int
	}{
		{name: "batch500_100KB", batch: 500, fieldSize: 100},
		{name: "batch50_1000KB", batch: 50, fieldSize: 1000},
	}

	client := &http.Client{}

	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			body := makeRunsBody(tc.batch, tc.fieldSize)
			url := ts.URL + "/runs"

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				resp, err := client.Post(url, "application/json", bytes.NewReader(body))
				if err != nil {
					b.Fatalf("POST /runs failed: %v", err)
				}
				if resp.StatusCode != http.StatusCreated {
					// Drain body to allow reuse of connection
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					b.Fatalf("expected status 201, got %d", resp.StatusCode)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		})
	}
}
