package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func deviceWithEntry(t *testing.T, upstream *httptest.Server) (*deviceManager, *deviceEntry) {
	t.Helper()
	m := newDeviceManager(upstream.Client(), filepath.Join(t.TempDir(), "device.json"))
	t.Cleanup(m.Close)
	now := time.Now()
	entry := &deviceEntry{ID: "cctv10-test", Slug: "cctv10", FinalURL: upstream.URL + "/live/main.m3u8",
		Headers: http.Header{"X-Media-Key": {"credential"}}, ExpiresAt: now.Add(time.Minute),
		SessionUntil: now.Add(time.Hour), MediaLeaseAt: now.Add(time.Minute)}
	m.entries[entry.Slug], m.mediaEntries[entry.ID] = entry, entry
	return m, entry
}

func awaitDeviceSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("device operation did not finish")
	}
}

func TestDeviceFallbackTimelineAndJCERecovery(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv1"]
	ch.workerRunning = true // Isolate response selection from the refresh worker.
	ch.mode = "jce"
	ch.segments = map[string]*segment{"jce": {Sequence: 42, Duration: 6, URL: "https://cdn.example/jce.ts"}}
	ch.order = []string{"jce"}
	ch.lastOK, ch.lastProgress = time.Now().Add(-time.Minute), time.Now().Add(-time.Minute)
	ch.lastError = "primary source unavailable"
	ch.nextRetry = time.Now().Add(time.Minute)
	calls := 0
	app := newAppServer(m, "127.0.0.1", 8767)
	app.device = &stubDeviceSource{fetch: func(context.Context, string, string) (string, error) {
		calls++
		return liveWindow(10, "device"), nil
	}}
	get := func() string {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", "/cctv1.m3u8", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	first := get()
	if second := get(); first != second {
		t.Fatalf("unchanged device response changed timeline:\n%s\n%s", first, second)
	}
	if p := mediaPosition(first); p.sequence != 10 || p.epoch != 0 {
		t.Fatalf("unused JCE contaminated first response: %+v", p)
	}
	// A recovered primary source takes precedence even though the device is healthy.
	ch.lastOK, ch.lastProgress = time.Now(), time.Now()
	ch.lastError = ""
	fallback := get()
	if !strings.Contains(fallback, "jce.ts") || !strings.Contains(fallback, "#EXT-X-DISCONTINUITY\n") {
		t.Fatalf("actual switch to JCE lost its boundary: %s", fallback)
	}
	if get() != fallback {
		t.Fatal("repeated JCE response changed timeline")
	}
	if calls != 2 {
		t.Fatalf("JCE recovery continued fetching the device source: calls=%d", calls)
	}
}

func TestDeviceSignaturesAndEntryRotationKeepMediaIdentity(t *testing.T) {
	m := newDeviceManager(http.DefaultClient, "")
	defer m.Close()
	var timeline playlistTimeline
	render := func(entry, host, sign, path string) string {
		input := strings.ReplaceAll(liveWindow(10, path), ".ts", ".ts?sign="+sign)
		relay := rewriteDevicePlaylist(input, "https://cdn.cctv.cn/live.m3u8", host, entry, m.signMediaURL)
		return timeline.normalizeWithIdentity(relay, "device-go", m.MediaIdentity)
	}
	first := render("old-entry", "localhost:8767", "old", "device")
	renewed := render("new-entry", "tv.local:8767", "new", "device")
	if first == renewed {
		t.Fatal("test did not change relay credentials")
	}
	if mediaPosition(first) != mediaPosition(renewed) || strings.Contains(renewed, "#EXT-X-DISCONTINUITY\n") {
		t.Fatalf("renewal changed media position:\n%s\n%s", first, renewed)
	}
	changed := render("new-entry", "tv.local:8767", "new", "different")
	if mediaPosition(changed).sequence != 13 || !strings.Contains(changed, "#EXT-X-DISCONTINUITY\n") {
		t.Fatalf("real content change did not create a boundary: %s", changed)
	}
}

func TestDeviceCanceledViewerDoesNotCancelSharedRefresh(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	defer once.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			io.WriteString(w, testPlaylist)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	m, entry := deviceWithEntry(t, upstream)
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() { _, err := m.FetchPlaylist(ctx, entry.Slug, "first.local:8767"); canceled <- err }()
	awaitDeviceSignal(t, started)
	cancel()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("viewer cancellation waited for shared refresh")
	}
	result := make(chan error, 1)
	go func() {
		p, err := m.FetchPlaylist(context.Background(), entry.Slug, "second.local:8767")
		if err == nil && !strings.Contains(p, "http://second.local:8767/") {
			err = errors.New("wrong public host")
		}
		result <- err
	}()
	once.Do(func() { close(release) })
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	failures := len(m.failures)
	m.mu.Unlock()
	if calls.Load() != 1 || failures != 0 {
		t.Fatalf("calls=%d failures=%d", calls.Load(), failures)
	}
	canceledCtx, stop := context.WithCancel(context.Background())
	stop()
	if _, err := m.FetchPlaylist(canceledCtx, entry.Slug, "localhost:8767"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767"); err != nil {
		t.Fatalf("canceled cache hit poisoned next viewer: %v", err)
	}
}

func TestDeviceConcurrentRefreshIsShared(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-release:
			io.WriteString(w, testPlaylist)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	m, entry := deviceWithEntry(t, upstream)
	defer once.Do(func() { close(release) })
	const viewers = 16
	ready := make(chan struct{}, viewers)
	results := make(chan error, viewers)
	for i := 0; i < viewers; i++ {
		go func(i int) {
			ready <- struct{}{}
			host := fmt.Sprintf("viewer%d.local:8767", i)
			p, err := m.FetchPlaylist(context.Background(), entry.Slug, host)
			if err == nil && !strings.Contains(p, "http://"+host+"/") {
				err = errors.New("shared response used another viewer's host")
			}
			results <- err
		}(i)
	}
	for i := 0; i < viewers; i++ {
		<-ready
	}
	waitFor(t, func() bool { return calls.Load() >= 1 })
	once.Do(func() { close(release) })
	for i := 0; i < viewers; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent viewers caused %d upstream requests", calls.Load())
	}
	m.mu.Lock()
	entry.PlaylistAt = time.Time{}
	m.mu.Unlock()
	if _, err := m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("completed flight prevented the next refresh")
	}
}

func TestDeviceControlQueueHonorsCancellation(t *testing.T) {
	m := newDeviceManager(http.DefaultClient, filepath.Join(t.TempDir(), "device.json"))
	defer m.Close()
	if err := m.acquireControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.releaseControl()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := m.resolveEntry(ctx, "cctv10"); result <- err }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("control queue ignored request deadline")
	}
}

func TestDeviceCloseCancelsAndJoinsRefresh(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	t.Cleanup(upstream.Close)
	m, entry := deviceWithEntry(t, upstream)
	done := make(chan struct{})
	go func() { defer close(done); m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767") }()
	awaitDeviceSignal(t, started)
	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()
	awaitDeviceSignal(t, closed)
	awaitDeviceSignal(t, done)
	m.mu.Lock()
	flights, failures := len(m.flights), len(m.failures)
	m.mu.Unlock()
	if flights != 0 || failures != 0 {
		t.Fatalf("shutdown left flights=%d failures=%d", flights, failures)
	}
	if _, err := m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767"); !errors.Is(err, context.Canceled) {
		t.Fatalf("new request after Close: %v", err)
	}
}

func TestDeviceProgressSurvivesEntryAndSessionRenewal(t *testing.T) {
	var sequence atomic.Int32
	sequence.Store(42)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, liveWindow(int(sequence.Load()), "device"))
	}))
	t.Cleanup(upstream.Close)
	m, entry := deviceWithEntry(t, upstream)
	if _, err := m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	state := m.progress[entry.Slug]
	state.LastProgress = time.Now().Add(-55 * time.Second)
	m.progress[entry.Slug] = state
	// Keep the two-second cache fresh: even cached data must obey the progress bound.
	m.mu.Unlock()
	if _, err := m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767"); err == nil || !strings.Contains(err.Error(), "stopped advancing") {
		t.Fatalf("frozen cache accepted: %v", err)
	}
	m.mu.Lock()
	session := &deviceSession{CreatedAt: time.Now()}
	m.session = session
	m.invalidateSessionLocked(session)
	replacement := *entry
	replacement.ID = "renewed-entry"
	replacement.PlaylistAt = time.Time{}
	replacement.PlaylistKey = ""
	m.entries[entry.Slug] = &replacement
	m.mediaEntries[replacement.ID] = &replacement
	delete(m.failures, entry.Slug)
	m.mu.Unlock()
	if _, err := m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767"); err == nil || !strings.Contains(err.Error(), "stopped advancing") {
		t.Fatalf("renewal erased frozen history: %v", err)
	}
	sequence.Store(43)
	m.mu.Lock()
	delete(m.failures, entry.Slug)
	m.mu.Unlock()
	if _, err := m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767"); err != nil {
		t.Fatalf("progress did not recover: %v", err)
	}
	sequence.Store(40)
	m.mu.Lock()
	replacement.PlaylistAt = time.Time{}
	m.mu.Unlock()
	if _, err := m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767"); err == nil || !strings.Contains(err.Error(), "backwards") {
		t.Fatalf("rollback accepted: %v", err)
	}
}

func TestDeviceEncoderResetNeedsAdvancingConfirmation(t *testing.T) {
	m := newDeviceManager(http.DefaultClient, "")
	defer m.Close()
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.acceptProgressLocked("cctv10", liveWindow(1000, "old"), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := m.acceptProgressLocked("cctv10", liveWindow(1, "new"), now); err == nil {
		t.Fatal("unconfirmed reset accepted")
	}
	if err := m.acceptProgressLocked("cctv10", liveWindow(1, "new"), now.Add(time.Second)); err == nil {
		t.Fatal("frozen reset accepted")
	}
	if err := m.acceptProgressLocked("cctv10", liveWindow(2, "new"), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceKEYAndMAPUseAuthenticatedRelay(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Media-Key") != "credential" {
			http.Error(w, "missing credentials", 403)
			return
		}
		switch r.URL.Path {
		case "/live/main.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:10\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin?token=1\",IV=0x00000000000000000000000000000001\n#EXT-X-MAP:URI=\"init.mp4\",BYTERANGE=\"16@0\"\n#EXTINF:4,\nsegment.m4s\n")
		case "/live/key.bin":
			if r.URL.Query().Get("token") != "1" {
				http.Error(w, "lost signature", 403)
				return
			}
			io.WriteString(w, "0123456789abcdef")
		case "/live/init.mp4":
			if r.Header.Get("Range") != "bytes=0-15" {
				http.Error(w, "lost range", 400)
				return
			}
			w.Header().Set("Content-Range", "bytes 0-15/32")
			w.WriteHeader(206)
			io.WriteString(w, "initialization!!")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	m, entry := deviceWithEntry(t, upstream)
	playlist, err := m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, line := range strings.Split(playlist, "\n") {
		if !strings.HasPrefix(line, "#EXT-X-KEY:") && !strings.HasPrefix(line, "#EXT-X-MAP:") {
			continue
		}
		_, raw, _ := strings.Cut(line, ":")
		attrs, err := playlistAttributes(raw)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(attrs["URI"])
		if err != nil || u.Host != "localhost:8767" || !strings.HasPrefix(u.Path, "/_device/media/") {
			t.Fatalf("invalid relay URI: %s", line)
		}
		req := httptest.NewRequest("GET", u.RequestURI(), nil)
		expected := 200
		if strings.HasPrefix(line, "#EXT-X-MAP:") {
			expected = 206
			req.Header.Set("Range", "bytes=0-15")
			if attrs["BYTERANGE"] != "16@0" {
				t.Fatal("lost MAP range")
			}
		}
		w := httptest.NewRecorder()
		m.ServeMedia(w, req)
		if w.Code != expected || w.Body.Len() != 16 {
			t.Fatalf("relay status=%d bytes=%d", w.Code, w.Body.Len())
		}
		checked++
	}
	if checked != 2 {
		t.Fatal("missing KEY/MAP")
	}
}

func TestDeviceRejectsUnsupportedEncryption(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key.bin\"\n#EXTINF:4,\nsegment.ts\n")
	}))
	t.Cleanup(upstream.Close)
	m, entry := deviceWithEntry(t, upstream)
	if _, err := m.FetchPlaylist(context.Background(), entry.Slug, "localhost:8767"); err == nil {
		t.Fatal("unsupported encryption advertised as playable")
	}
}

func TestDeviceMediaSlowReaderCanceledAndCapacityReleased(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				chunk := make([]byte, 128<<10)
				for i := 0; i < 512; i++ {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
			}))
			t.Cleanup(upstream.Close)
			m, entry := deviceWithEntry(t, upstream)
			done, started := make(chan struct{}), make(chan struct{})
			requestCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				close(started)
				m.ServeMedia(w, r.WithContext(requestCtx))
			}))
			server.Config.WriteTimeout = 100 * time.Millisecond
			server.Start()
			defer server.Close()
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.(*net.TCPConn).SetReadBuffer(1024)
			io.WriteString(conn, "GET /_device/media/"+m.signMediaURL(entry.ID, upstream.URL+"/segment.ts")+" HTTP/1.1\r\nHost: localhost\r\n\r\n")
			awaitDeviceSignal(t, started)
			// Fill the downstream send buffer; the client deliberately never reads.
			time.Sleep(200 * time.Millisecond)
			select {
			case <-done:
				t.Fatal("transfer ended before cancellation; slow-reader case not exercised")
			default:
			}
			if shutdown {
				closed := make(chan struct{})
				go func() { m.Close(); close(closed) }()
				awaitDeviceSignal(t, closed)
			} else {
				cancel()
			}
			awaitDeviceSignal(t, done)
			if len(m.mediaSlots) != 0 {
				t.Fatal("media concurrency slot leaked")
			}
		})
	}
}

func TestDeviceMediaConcurrencyIsBounded(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, "media") }))
	t.Cleanup(upstream.Close)
	m, entry := deviceWithEntry(t, upstream)
	for i := 0; i < cap(m.mediaSlots); i++ {
		m.mediaSlots <- struct{}{}
	}
	defer func() {
		for len(m.mediaSlots) > 0 {
			<-m.mediaSlots
		}
	}()
	req := httptest.NewRequest("GET", "/_device/media/"+m.signMediaURL(entry.ID, upstream.URL+"/segment.ts"), nil)
	w := httptest.NewRecorder()
	m.ServeMedia(w, req)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" || calls.Load() != 0 {
		t.Fatalf("capacity rejection status=%d calls=%d", w.Code, calls.Load())
	}
	<-m.mediaSlots
	w = httptest.NewRecorder()
	m.ServeMedia(w, req)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("released capacity did not recover: status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestDeviceFallbackSharesTotalRequestBudget(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv10"]
	// A shared primary refresh is already running; this viewer waits on it.
	ch.workerRunning, ch.refreshing = true, true
	app := newAppServer(m, "127.0.0.1", 8767)
	const total = 200 * time.Millisecond
	const reserve = 80 * time.Millisecond
	start := time.Now()
	var fallbackDeadline time.Time
	app.device = &stubDeviceSource{fetch: func(ctx context.Context, slug, host string) (string, error) {
		fallbackDeadline, _ = ctx.Deadline()
		<-ctx.Done()
		return "", ctx.Err()
	}}
	w := httptest.NewRecorder()
	app.serveChannelWithBudget(w, httptest.NewRequest("GET", "/cctv10.m3u8", nil), "cctv10", total, reserve)
	// Compare the assigned deadline, not just elapsed time, to avoid scheduler-sensitive timing assertions.
	if fallbackDeadline.IsZero() || fallbackDeadline.Sub(start) > total+30*time.Millisecond {
		t.Fatalf("device fallback extended total budget: %v", fallbackDeadline.Sub(start))
	}
	if w.Code != 503 || app.deviceFailures != 0 {
		t.Fatalf("viewer timeout status=%d bootstrap failures=%d", w.Code, app.deviceFailures)
	}
	if !ch.workerRunning {
		t.Fatal("viewer timeout stopped shared primary refresh")
	}
}

func TestDeviceMediaRequestDeadlineInterruptsSlowWriter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 128<<10)
		for i := 0; i < 512; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	m, entry := deviceWithEntry(t, upstream)
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); m.ServeMedia(w, r.WithContext(ctx)) }))
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.(*net.TCPConn).SetReadBuffer(1024)
	io.WriteString(conn, "GET /_device/media/"+m.signMediaURL(entry.ID, upstream.URL+"/segment.ts")+" HTTP/1.1\r\nHost: localhost\r\n\r\n")
	<-ctx.Done()
	awaitDeviceSignal(t, done)
	if len(m.mediaSlots) != 0 {
		t.Fatal("deadline leaked media capacity")
	}
}

func TestDeviceMediaTokenRejectsNonCanonicalEncoding(t *testing.T) {
	m := newDeviceManager(http.DefaultClient, "")
	defer m.Close()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	checked := 0
	for n := 0; n < 3; n++ {
		token := m.signMediaURL("entry", "https://cdn.cctv.cn/segment.ts"+strings.Repeat("x", n))
		if _, _, err := m.decodeMediaURL(token); err != nil {
			t.Fatal(err)
		}
		if len(token)%4 == 0 {
			continue
		}
		// Only unused padding bits change; a permissive decoder returns identical bytes.
		last := strings.IndexByte(alphabet, token[len(token)-1])
		alternate := token[:len(token)-1] + string(alphabet[last+1])
		if _, _, err := m.decodeMediaURL(alternate); err == nil {
			t.Fatal("nonzero Base64 padding bits accepted")
		}
		if _, _, err := m.decodeMediaURL(token + "\n"); err == nil {
			t.Fatal("noncanonical newline accepted")
		}
		checked++
	}
	if checked != 2 {
		t.Fatalf("checked %d padding cases", checked)
	}
}
