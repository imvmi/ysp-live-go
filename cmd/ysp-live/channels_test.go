package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testPlaylist = "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:42\n#EXTINF:6,\nfresh.ts\n"

// Change only the destination, then perform real HTTP with Go's Transport.
// No upstream response is synthesized inside the client.
type testRoutingTransport struct {
	base      *url.URL
	transport *http.Transport
}

func (r *testRoutingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copyReq := req.Clone(req.Context())
	u := *req.URL
	u.Scheme, u.Host = r.base.Scheme, r.base.Host
	u.Path = strings.TrimRight(r.base.Path, "/") + req.URL.Path
	copyReq.URL = &u
	copyReq.Header.Set("X-Upstream-Host", req.URL.Hostname())
	copyReq.Host = r.base.Host
	return r.transport.RoundTrip(copyReq)
}

func testManager(t *testing.T, upstream string) (*channelManager, *channel) {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	m := newChannelManager(&http.Client{Transport: &testRoutingTransport{u, transport}, Timeout: 2 * time.Second})
	m.refreshInterval = time.Hour
	t.Cleanup(m.Close)
	ch := m.channels["cctv11"]
	ch.bkURLs = []string{"https://bklive-test.ysp.cctv.cn/live.m3u8"}
	ch.bkURLsTime = time.Now()
	// Reproduce reopening a previously viewed channel after six months.
	ch.bkPlaylist = "#EXTM3U\n#EXTINF:6,\nhttps://old.invalid/expired.ts\n"
	ch.lastOK = time.Now().Add(-180 * 24 * time.Hour)
	return m, ch
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestIdleReopenSharesRefresh(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	defer once.Do(func() { close(release) })
	m, ch := testManager(t, upstream.URL)
	app := newAppServer(m, nil, "127.0.0.1", 8767)
	results := make(chan *httptest.ResponseRecorder, 24)
	for i := 0; i < cap(results); i++ {
		go func() {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest("GET", "/cctv11.m3u8", nil))
			results <- w
		}()
	}
	waitFor(t, func() bool { return calls.Load() > 0 })
	select {
	case w := <-results:
		t.Fatalf("returned before refresh: %d %s", w.Code, w.Body)
	default:
	}
	once.Do(func() { close(release) })
	for i := 0; i < cap(results); i++ {
		select {
		case w := <-results:
			if w.Code != 200 || !strings.Contains(w.Body.String(), "fresh.ts") || strings.Contains(w.Body.String(), "expired.ts") {
				t.Fatalf("unexpected response: %d %s", w.Code, w.Body)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("client did not finish")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d upstream calls for one shared refresh", calls.Load())
	}
	if got := m.buildPlaylist(ch); !strings.Contains(got, "fresh.ts") {
		t.Fatal(got)
	}
}

func TestCanceledViewerDoesNotCancelOtherViewers(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	defer once.Do(func() { close(release) })
	m, ch := testManager(t, upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- m.ensure(ctx, ch) }()
	<-started
	second := make(chan error, 1)
	go func() { second <- m.ensure(context.Background(), ch) }()
	cancel()
	if err := <-first; err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	once.Do(func() { close(release) })
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

func TestStaleFailureBackoffAndRecovery(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing.Load() {
			http.Error(w, "upstream unavailable", 503)
			return
		}
		if r.URL.Path == "/" {
			_, _ = io.WriteString(w, `{"iretcode":0,"playurl":"https://bklive-test.ysp.cctv.cn/live.m3u8"}`)
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	app := newAppServer(m, nil, "127.0.0.1", 8767)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/cctv11.m3u8", nil))
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d headers %v", w.Code, w.Header())
	}
	if m.buildPlaylist(ch) != "" {
		t.Fatal("stale cache was served after failure")
	}
	count := calls.Load()
	for i := 0; i < 30; i++ {
		if m.ensure(context.Background(), ch) == nil {
			t.Fatal("expected failure")
		}
	}
	if calls.Load() != count {
		t.Fatal("requests bypassed shared retry backoff")
	}
	failing.Store(false)
	ch.mu.Lock()
	ch.nextRetry = time.Time{}
	ch.mu.Unlock()
	if err := m.ensure(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.buildPlaylist(ch), "fresh.ts") {
		t.Fatal("did not recover")
	}
}

func TestCloseCancelsRefreshAndRejectsNewWorkers(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	finished := make(chan error, 1)
	go func() { finished <- m.ensure(context.Background(), ch) }()
	<-started
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel in-flight HTTP")
	}
	if err := <-finished; err == nil {
		t.Fatal("expected cancellation")
	}
	if err := m.ensure(context.Background(), ch); err != context.Canceled {
		t.Fatalf("ensure after Close: %v", err)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.workerRunning {
		t.Fatal("worker survived shutdown")
	}
}

func TestIdleWorkerCanRestart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, testPlaylist) }))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	m.idleTimeout = 10 * time.Millisecond
	m.refreshInterval = 5 * time.Millisecond
	for i := 0; i < 5; i++ {
		if err := m.ensure(context.Background(), ch); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { ch.mu.Lock(); defer ch.mu.Unlock(); return !ch.workerRunning })
		ch.mu.Lock()
		ch.lastOK = time.Now().Add(-time.Hour)
		ch.mu.Unlock()
	}
}

func TestSegmentWindowRemainsBoundedAndOrdered(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv1"]
	makeSegments := func(start int) []segment {
		items := make([]segment, 100)
		for i := range items {
			items[i] = segment{Duration: 3, URL: fmt.Sprintf("https://cdn.test/%d.ts?token=new", start+i)}
		}
		return items
	}
	for poll := 0; poll < 10000; poll++ {
		m.mergeSegments(ch, makeSegments(poll))
		if len(ch.order) != 60 || len(ch.segments) != 60 {
			t.Fatal("unbounded cache")
		}
		if ch.sequence != uint64(60+poll) {
			t.Fatalf("poll %d: sequence %d; old segments re-added", poll, ch.sequence)
		}
	}
	seq := ch.sequence
	m.mergeSegments(ch, makeSegments(9999))
	if ch.sequence != seq {
		t.Fatal("identical window advanced sequence")
	}
	m.mergeSegments(ch, makeSegments(20000))
	if ch.segments[ch.order[0]].Epoch != 1 {
		t.Fatal("media gap did not advance discontinuity sequence")
	}
	ch.lastOK = time.Now()
	ch.lastProgress = time.Now()
	if !strings.Contains(m.buildPlaylist(ch), "#EXT-X-DISCONTINUITY-SEQUENCE:1") {
		t.Fatal("missing discontinuity sequence")
	}
}

func TestFrozenPlaylistIsNotHealthy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, testPlaylist) }))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	// Exercise the actual HTTP refresh without a background timer.
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal("initial fetch failed")
	}
	ch.mu.Lock()
	ch.lastProgress = time.Now().Add(-time.Hour)
	ch.mu.Unlock()
	if m.refreshOnce(context.Background(), ch) {
		t.Fatal("HTTP 200 from a frozen stream counted as healthy")
	}
	if m.buildPlaylist(ch) != "" {
		t.Fatal("frozen playlist escaped")
	}
	if !strings.Contains(ch.lastError, "stopped advancing") {
		t.Fatal(ch.lastError)
	}
}

func TestDiscoveryOutageKeepsWorkingCDNCandidate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Error(w, "discovery outage", 503)
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	ch.bkURLsTime = time.Time{}
	if err := m.ensure(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.buildPlaylist(ch), "fresh.ts") {
		t.Fatal("working CDN lost during discovery outage")
	}
}

func TestHungUpstreamRespectsClientTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	m.client.Timeout = 20 * time.Millisecond
	started := time.Now()
	if err := m.ensure(context.Background(), ch); err == nil {
		t.Fatal("hung upstream succeeded")
	}
	if time.Since(started) > time.Second {
		t.Fatal("upstream timeout was not bounded")
	}
	if m.buildPlaylist(ch) != "" {
		t.Fatal("served expired cache after timeout")
	}
}

func TestJCERefreshAndFallbackOverHTTP(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint("fallback=", fallback), func(t *testing.T) {
			playlistURL := "https://bklive-test.ysp.cctv.cn/live.m3u8"
			if fallback {
				playlistURL = "https://liverecord.video.cloud.cctv.com/live.m3u8"
			}
			body := &jceWriter{}
			body.integer(0, 0)
			body.str(playlistURL, 2)
			packet, err := wrapJCE(pidTimeShift, body.b, 1, strings.Repeat("a", 32))
			if err != nil {
				t.Fatal(err)
			}
			var jceCalls, cdnCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Header.Get("X-Upstream-Host") {
				case "jacc.ysp.cctv.cn":
					jceCalls.Add(1)
					if r.Method != "POST" {
						t.Error("JCE request is not POST")
					}
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = w.Write(packet)
				case "bkliveinfo.ysp.cctv.cn":
					_, _ = io.WriteString(w, `{"iretcode":0,"playurl":"https://bklive-test.ysp.cctv.cn/live.m3u8"}`)
				default:
					cdnCalls.Add(1)
					_, _ = io.WriteString(w, testPlaylist)
				}
			}))
			defer upstream.Close()
			m, _ := testManager(t, upstream.URL)
			ch := m.channels["cctv1"]
			if err := m.mergeSegments(ch, []segment{{URL: "https://old.invalid/expired.ts", Duration: 6}}); err != nil {
				t.Fatal(err)
			}
			ch.lastOK = time.Now().Add(-180 * 24 * time.Hour)
			if err := m.ensure(context.Background(), ch); err != nil {
				t.Fatal(err)
			}
			playlist := m.buildPlaylist(ch)
			if !strings.Contains(playlist, "fresh.ts") || strings.Contains(playlist, "expired.ts") {
				t.Fatal(playlist)
			}
			if jceCalls.Load() != 1 || cdnCalls.Load() != 1 {
				t.Fatalf("calls: JCE=%d CDN=%d", jceCalls.Load(), cdnCalls.Load())
			}
			ch.mu.Lock()
			mode := ch.mode
			ch.mu.Unlock()
			if (mode == "bk") != fallback {
				t.Fatalf("mode=%s", mode)
			}
		})
	}
}

// Run with HLS_SIMULATION_URL against the committed Postman simulation.
func TestPostmanSimulation(t *testing.T) {
	base := os.Getenv("HLS_SIMULATION_URL")
	if base == "" {
		t.Skip("set HLS_SIMULATION_URL to run the Postman HTTP simulation")
	}
	m, ch := testManager(t, base)
	app := httptest.NewServer(newAppServer(m, nil, "127.0.0.1", 8767))
	defer app.Close()
	control := func(mode string) {
		resp, err := http.Post(strings.TrimRight(base, "/")+"/control?mode="+mode, "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("control HTTP %d", resp.StatusCode)
		}
	}
	request := func(status int) {
		resp, err := http.Get(app.URL + "/cctv11.m3u8")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != status {
			t.Fatalf("got %d, want %d: %s", resp.StatusCode, status, body)
		}
		if strings.Contains(string(body), "expired.ts") {
			t.Fatal("expired cache escaped")
		}
	}
	control("ok")
	request(200)
	control("error")
	ch.mu.Lock()
	ch.lastOK = time.Now().Add(-time.Hour)
	ch.nextRetry = time.Time{}
	ch.mu.Unlock()
	request(503)
	control("ok")
	ch.mu.Lock()
	ch.nextRetry = time.Time{}
	ch.mu.Unlock()
	request(200)
}
