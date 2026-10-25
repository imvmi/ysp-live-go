package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func expireSourceCooldowns(ch *channel) {
	// Advance the retry state without sleeping for the production cooldown.
	for u, state := range ch.bkSources {
		state.RetryAfter = time.Time{}
		ch.bkSources[u] = state
	}
}

type observedTransport struct {
	base   http.RoundTripper
	active atomic.Int32
	peak   atomic.Int32
}

func (tr *observedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	n := tr.active.Add(1)
	defer tr.active.Add(-1)
	for old := tr.peak.Load(); n > old && !tr.peak.CompareAndSwap(old, n); old = tr.peak.Load() {
	}
	return tr.base.RoundTrip(req)
}

func TestColdLoadAndChannelSwitchAvoidSlowCDN(t *testing.T) {
	var slow, fast, active atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		if r.URL.Path == "/slow.m3u8" {
			slow.Add(1)
			<-r.Context().Done()
			return
		}
		fast.Add(1)
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	urls := []string{"https://bklive-test.ysp.cctv.cn/slow.m3u8", "https://bklive-test.ysp.cctv.cn/fast.m3u8"}
	m.setBKURLs(ch, urls)
	m.setBKURLs(m.channels["cctv12"], urls)
	app := httptest.NewServer(newAppServer(m, nil, "127.0.0.1", 8767))
	defer app.Close()
	get := func(slug string) time.Duration {
		t.Helper()
		started := time.Now()
		resp, err := http.Get(app.URL + "/" + slug + ".m3u8")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "fresh.ts") {
			t.Fatalf("%d %s %v", resp.StatusCode, body, err)
		}
		return time.Since(started)
	}
	cold := get("cctv11")
	other := get("cctv12")
	before := fast.Load()
	warm := get("cctv11")
	if fast.Load() != before {
		t.Fatal("switching back ignored fresh cache")
	}
	ch.mu.Lock()
	ch.lastOK = time.Now().Add(-time.Minute)
	ch.mu.Unlock()
	refreshed := get("cctv11")
	waitFor(t, func() bool { return active.Load() == 0 })
	if slow.Load() != 2 {
		t.Fatalf("slow CDN retried after a winner was learned: %d", slow.Load())
	}
	if cold > time.Second || other > time.Second || warm > 500*time.Millisecond || refreshed > 500*time.Millisecond {
		t.Fatalf("cold=%s switch=%s warm=%s refresh=%s", cold, other, warm, refreshed)
	}
	t.Logf("cold=%s new-channel=%s switch-back=%s preferred-refresh=%s", cold.Round(time.Millisecond), other.Round(time.Millisecond), warm.Round(time.Millisecond), refreshed.Round(time.Millisecond))
}

func TestFrozenPrimarySwitchesToHealthyBackup(t *testing.T) {
	var frozen, good, discovery atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/frozen.m3u8":
			frozen.Add(1)
			_, _ = io.WriteString(w, testPlaylist)
		case "/good.m3u8":
			good.Add(1)
			_, _ = io.WriteString(w, strings.ReplaceAll(testPlaylist, "fresh.ts", "advancing.ts"))
		default:
			discovery.Add(1)
			http.Error(w, "unexpected discovery", 503)
		}
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	m.setBKURLs(ch, []string{"https://bklive-test.ysp.cctv.cn/frozen.m3u8", "https://bklive-test.ysp.cctv.cn/good.m3u8"})
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
	ch.lastProgress = time.Now().Add(-time.Minute)
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
	if !strings.Contains(m.buildPlaylist(ch), "advancing.ts") {
		t.Fatal("frozen primary blocked healthy backup")
	}
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
	if frozen.Load() != 2 || good.Load() != 2 || discovery.Load() != 0 {
		t.Fatalf("frozen=%d good=%d discovery=%d", frozen.Load(), good.Load(), discovery.Load())
	}
}

func TestCandidateConcurrencyIsBoundedAndLosersCancel(t *testing.T) {
	var active atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		if r.URL.Path != "/good.m3u8" {
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	observed := &observedTransport{base: m.client.Transport}
	m.client.Transport = observed
	m.hedgeDelay = 10 * time.Millisecond
	m.sourceTimeout = 80 * time.Millisecond
	m.setBKURLs(ch, []string{"https://cdn.cctv.cn/a", "https://cdn.cctv.cn/b", "https://cdn.cctv.cn/c", "https://cdn.cctv.cn/good.m3u8"})
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
	waitFor(t, func() bool { return active.Load() == 0 })
	// The hung sources never send headers, so their entire active request
	// lifetime is visible to the transport observer.
	if observed.peak.Load() != 2 || observed.active.Load() != 0 {
		t.Fatalf("candidate concurrency: peak=%d active=%d", observed.peak.Load(), observed.active.Load())
	}
}

func TestRenewedSignedURLCanRecoverBeforeCooldownExpires(t *testing.T) {
	var old, renewed atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = io.WriteString(w, `{"iretcode":0,"playurl":"https://bklive-test.ysp.cctv.cn/live.m3u8?token=new"}`)
			return
		}
		if r.URL.Query().Get("token") == "old" {
			old.Add(1)
			http.Error(w, "expired", 403)
			return
		}
		renewed.Add(1)
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	m.setBKURLs(ch, []string{"https://bklive-test.ysp.cctv.cn/live.m3u8?token=old"})
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
	if old.Load() != 1 || renewed.Load() != 1 {
		t.Fatalf("old=%d renewed=%d", old.Load(), renewed.Load())
	}
}

func TestSourceHealthBoundedAcrossSignedURLRenewals(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv11"]
	for generation := 0; generation < 1000; generation++ {
		var urls []string
		for i := 0; i < 100; i++ {
			urls = append(urls, fmt.Sprintf("https://cdn.cctv.cn/%d.m3u8?token=%d", i, generation))
		}
		m.setBKURLs(ch, urls)
		for _, u := range ch.bkURLs {
			m.failBKSource(ch, u, context.DeadlineExceeded)
		}
		if len(ch.bkURLs) > maxCDNCandidates || len(ch.bkSources) > maxCDNCandidates {
			t.Fatal("source history accumulated across renewals")
		}
	}
}

func TestSourceRecoversAfterCooldown(t *testing.T) {
	var failed atomic.Bool
	failed.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failed.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	m.sourceCooldown = 20 * time.Millisecond
	if m.refreshOnce(context.Background(), ch) {
		t.Fatal("expected failure")
	}
	failed.Store(false)
	time.Sleep(25 * time.Millisecond)
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
}

func TestRenewedSignatureDoesNotResetFrozenHistory(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		playlist := testPlaylist
		if r.URL.Path == "/good.m3u8" {
			playlist = strings.ReplaceAll(playlist, "fresh.ts", "new.ts")
		}
		_, _ = io.WriteString(w, playlist)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	oldURL := "https://cdn.cctv.cn/frozen.m3u8?token=old"
	newURL := "https://cdn.cctv.cn/frozen.m3u8?token=new"
	m.setBKURLs(ch, []string{oldURL})
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
	state := ch.bkSources[oldURL]
	state.LastProgress = time.Now().Add(-time.Minute)
	ch.bkSources[oldURL] = state
	ch.lastMedia = "a different previously served source"
	m.setBKURLs(ch, []string{newURL, "https://cdn.cctv.cn/good.m3u8"})
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
	if !strings.Contains(m.buildPlaylist(ch), "new.ts") {
		t.Fatal("renewed signature disguised a frozen source")
	}
}

func TestPreferredSignedURLWinsOverSamePathCandidate(t *testing.T) {
	var slow atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") == "slow" {
			slow.Add(1)
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	m.hedgeDelay = 20 * time.Millisecond
	m.setBKURLs(ch, []string{"https://cdn.cctv.cn/live.m3u8?token=slow", "https://cdn.cctv.cn/live.m3u8?token=good"})
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
	expireSourceCooldowns(ch)
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
	if slow.Load() != 1 {
		t.Fatal("successful signed URL lost priority after cooldown")
	}
}

func TestCloseCancelsBothHedgedRequests(t *testing.T) {
	var active atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	m.hedgeDelay = 10 * time.Millisecond
	m.setBKURLs(ch, []string{"https://cdn.cctv.cn/a", "https://cdn.cctv.cn/b"})
	result := make(chan error, 1)
	go func() { result <- m.ensure(context.Background(), ch) }()
	waitFor(t, func() bool { return active.Load() == 2 })
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hedged requests survived Close")
	}
	if err := <-result; err == nil {
		t.Fatal("expected cancellation")
	}
	waitFor(t, func() bool { return active.Load() == 0 })
}
