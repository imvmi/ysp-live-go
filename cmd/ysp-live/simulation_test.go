package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostmanLatencyAndSourceSelection(t *testing.T) {
	base := os.Getenv("HLS_SIMULATION_URL")
	if base == "" {
		t.Skip("set HLS_SIMULATION_URL to run the Postman HTTP simulation")
	}
	for _, mode := range []string{"slow-primary", "frozen-primary", "quality"} {
		t.Run(mode, func(t *testing.T) {
			resp, err := http.Post(strings.TrimRight(base, "/")+"/control?mode="+mode, "text/plain", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatal(resp.StatusCode)
			}
			m, ch := testManager(t, base)
			m.client.Timeout = 35 * time.Second
			// Include discovery in the first-load timing.
			m.setBKURLs(ch, nil)
			app := httptest.NewServer(newAppServer(m, nil, "127.0.0.1", 8767))
			defer app.Close()
			get := func() (string, time.Duration) {
				t.Helper()
				started := time.Now()
				r, err := http.Get(app.URL + "/cctv11.m3u8")
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if err != nil || r.StatusCode != 200 {
					t.Fatalf("HTTP %d: %s (%v)", r.StatusCode, b, err)
				}
				return string(b), time.Since(started)
			}
			first, cold := get()
			switch mode {
			case "slow-primary":
				if !strings.Contains(first, "healthy.ts") || cold > 1500*time.Millisecond || cold < m.hedgeDelay/2 {
					t.Fatalf("cold %s: %s", cold, first)
				}
				ch.mu.Lock()
				ch.lastOK = time.Now().Add(-time.Minute)
				ch.mu.Unlock()
				second, preferred := get()
				_, cached := get()
				if !strings.Contains(second, "healthy.ts") || preferred > 500*time.Millisecond || cached > 500*time.Millisecond {
					t.Fatalf("preferred=%s cached=%s", preferred, cached)
				}
				t.Logf("cold including discovery=%s preferred refresh=%s cached=%s", cold.Round(time.Millisecond), preferred.Round(time.Millisecond), cached.Round(time.Millisecond))
			case "frozen-primary":
				if !strings.Contains(first, "frozen.ts") {
					t.Fatal(first)
				}
				ch.mu.Lock()
				ch.lastOK = time.Now().Add(-time.Minute)
				ch.lastProgress = time.Now().Add(-time.Minute)
				ch.mu.Unlock()
				second, elapsed := get()
				if !strings.Contains(second, "healthy.ts") {
					t.Fatal(second)
				}
				t.Logf("frozen-to-healthy failover=%s", elapsed.Round(time.Millisecond))
			case "quality":
				if !strings.Contains(first, "low.ts") || strings.Contains(first, "high.ts") {
					t.Fatal(first)
				}
			}
		})
	}
}
