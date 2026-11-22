package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleReopenGetsColdDiscoveryBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Upstream-Host") == "bkliveinfo.ysp.cctv.cn" {
			_, _ = io.WriteString(w, `{"iretcode":0,"playurl":"https://cdn.cctv.cn/live.m3u8"}`)
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	ch.lastOK = time.Now().Add(-30 * 24 * time.Hour)
	ch.bkURLs = nil
	m.bkSlots <- struct{}{}
	release := time.AfterFunc(9*time.Second, func() { <-m.bkSlots })
	defer release.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := m.ensure(ctx, ch); err != nil {
		t.Fatal("previously watched idle channel used short active-viewer budget:", err)
	}
	if m.buildPlaylist(ch) == "" {
		t.Fatal("idle channel did not recover")
	}
}

func TestResetConfirmationSurvivesSignatureRenewal(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv11"]
	first, second := "https://cdn.cctv.cn/live.m3u8?token=1", "https://cdn.cctv.cn/live.m3u8?token=2"
	m.setBKURLs(ch, []string{first})
	if err := m.acceptBKSource(ch, playlistResult{url: first, playlist: liveWindow(10000, "old")}); err != nil {
		t.Fatal(err)
	}
	ch.bkLastProgress = time.Now().Add(-time.Minute)
	if err := m.acceptBKSource(ch, playlistResult{url: first, playlist: liveWindow(1, "new")}); err == nil {
		t.Fatal("accepted unconfirmed reset")
	}
	m.setBKURLs(ch, []string{second})
	if err := m.acceptBKSource(ch, playlistResult{url: second, playlist: liveWindow(2, "new")}); err != nil {
		t.Fatal("signature renewal erased restart confirmation:", err)
	}
}

func TestBackendStartupTimeoutReapsUnreadyChild(t *testing.T) {
	t.Setenv("YSP_TEST_BACKEND_PROCESS", "1")
	t.Setenv("YSP_TEST_BACKEND_UNREADY", "1")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	listener.Close()
	port, _ := strconv.Atoi(portText)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b := newBackendManager(&http.Client{Timeout: time.Second}, exe, port, t.TempDir())
	defer b.Stop()
	for attempt := 0; attempt < 5; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		err = b.Start(ctx)
		cancel()
		if err == nil {
			t.Fatal("unready child unexpectedly started")
		}
		b.mu.Lock()
		leftover := b.cmd != nil || b.logFile != nil || b.ready
		b.mu.Unlock()
		if leftover {
			t.Fatal("timed-out startup retained child or log handle")
		}
	}
	t.Setenv("YSP_TEST_BACKEND_UNREADY", "0")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil || !b.Ready() {
		t.Fatal("could not start a replacement child", err)
	}
}

func TestResetConfirmationCannotCrossCDNOrExtendLifetime(t *testing.T) {
	for _, differentHost := range []bool{false, true} {
		m := newChannelManager(http.DefaultClient)
		ch := m.channels["cctv11"]
		first := "https://cdn.cctv.cn/live.m3u8?token=1"
		m.setBKURLs(ch, []string{first})
		ch.bkPlaylist = liveWindow(10000, "old")
		ch.bkLastProgress = time.Now().Add(-time.Hour)
		_ = m.acceptBKSource(ch, playlistResult{url: first, playlist: liveWindow(1, "new")})
		next := "https://cdn.cctv.cn/live.m3u8?token=2"
		if differentHost {
			next = "https://other.cctv.cn/live.m3u8?token=2"
		} else {
			state := ch.bkSources[first]
			state.ResetObserved = time.Now().Add(-time.Hour)
			ch.bkSources[first] = state
		}
		m.setBKURLs(ch, []string{next})
		err := m.acceptBKSource(ch, playlistResult{url: next, playlist: liveWindow(2, "new")})
		m.Close()
		if err == nil {
			t.Fatalf("invalid reset confirmation accepted, differentHost=%t", differentHost)
		}
	}
}

func TestRepeatedIdleRestartReleasesWorkers(t *testing.T) {
	var calls, active atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		_, _ = io.WriteString(w, liveWindow(int(calls.Add(1)), "live"))
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	m.idleTimeout = 50 * time.Millisecond
	m.refreshInterval = 20 * time.Millisecond
	for cycle := 0; cycle < 25; cycle++ {
		if err := m.ensure(context.Background(), ch); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { ch.mu.Lock(); defer ch.mu.Unlock(); return !ch.workerRunning })
		ch.mu.Lock()
		errState := ch.starting || ch.refreshing
		states := len(ch.bkSources)
		ch.mu.Unlock()
		if errState || active.Load() != 0 || states > maxCDNCandidates {
			t.Fatalf("cycle %d left active work or unbounded state", cycle)
		}
	}
	m.Close()
	if calls.Load() != 25 {
		t.Fatalf("expected one refresh per idle restart, got %d", calls.Load())
	}
}
