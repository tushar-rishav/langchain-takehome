package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// BenchmarkGetRun exercises the /runs/{id} handler under different dataset sizes
// and access patterns (sequential, round-robin, parallel, ETag hits).
func BenchmarkGetRun(b *testing.B) {
	r, _ := newTestRouter(b)
	ts := httptest.NewServer(r)
	defer ts.Close()

	client := &http.Client{}

	type datasetCase struct {
		name      string
		batch     int
		fieldSize int // KB per logical field map (inputs/outputs/metadata)
	}
	cases := []datasetCase{
		{name: "small_100x1KB", batch: 100, fieldSize: 1},
		{name: "large_10x500KB", batch: 10, fieldSize: 500},
	}

	for _, cs := range cases {
		// Prepare data: POST /runs once per case
		body := makeRunsBody(cs.batch, cs.fieldSize)
		resp, err := client.Post(ts.URL+"/runs", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatalf("prep POST /runs failed: %v", err)
		}
		if resp.StatusCode != http.StatusCreated {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			b.Fatalf("prep expected 201 got %d", resp.StatusCode)
		}
		var created struct {
			RunIDs []string `json:"run_ids"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			b.Fatalf("decode created: %v", err)
		}
		resp.Body.Close()
		if len(created.RunIDs) == 0 {
			b.Fatalf("no run ids returned for case %s", cs.name)
		}

		firstID := created.RunIDs[0]
		b.Run("sequential_"+cs.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rr, err := client.Get(ts.URL + "/runs/" + firstID)
				if err != nil {
					b.Fatalf("GET failed: %v", err)
				}
				io.Copy(io.Discard, rr.Body)
				rr.Body.Close()
				if rr.StatusCode != http.StatusOK {
					b.Fatalf("unexpected status %d", rr.StatusCode)
				}
			}
		})

		b.Run("round_robin_"+cs.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			idx := 0
			for i := 0; i < b.N; i++ {
				id := created.RunIDs[idx]
				idx++
				if idx == len(created.RunIDs) {
					idx = 0
				}
				rr, err := client.Get(ts.URL + "/runs/" + id)
				if err != nil {
					b.Fatalf("GET failed: %v", err)
				}
				io.Copy(io.Discard, rr.Body)
				rr.Body.Close()
				if rr.StatusCode != http.StatusOK {
					b.Fatalf("unexpected %d", rr.StatusCode)
				}
			}
		})

		b.Run("parallel_"+cs.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var ctr uint64
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					// pick id in striped manner to avoid contention on single S3 range
					idx := atomic.AddUint64(&ctr, 1) % uint64(len(created.RunIDs))
					id := created.RunIDs[idx]
					rr, err := client.Get(ts.URL + "/runs/" + id)
					if err != nil {
						b.Fatalf("GET failed: %v", err)
					}
					io.Copy(io.Discard, rr.Body)
					rr.Body.Close()
					if rr.StatusCode != http.StatusOK {
						b.Fatalf("unexpected %d", rr.StatusCode)
					}
				}
			})
		})

		// Random access pattern
		b.Run("random_"+cs.name, func(b *testing.B) {
			ids := created.RunIDs
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				id := ids[rand.Intn(len(ids))]
				rr, err := client.Get(ts.URL + "/runs/" + id)
				if err != nil {
					b.Fatalf("GET failed: %v", err)
				}
				io.Copy(io.Discard, rr.Body)
				rr.Body.Close()
				if rr.StatusCode != http.StatusOK {
					b.Fatalf("unexpected %d", rr.StatusCode)
				}
			}
		})
	}
}
