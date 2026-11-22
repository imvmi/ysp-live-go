package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A real child process exercises exec.Wait, concurrent readiness checks and
// log pipe cleanup without depending on the optional third-party binary.
func TestMain(m *testing.M) {
	if os.Getenv("YSP_TEST_BACKEND_PROCESS") == "1" {
		port := ""
		for i, arg := range os.Args {
			if arg == "--port" && i+1 < len(os.Args) {
				port = os.Args[i+1]
			}
		}
		mux := http.NewServeMux()
		var unhealthy atomic.Bool
		mux.HandleFunc("/4kfifa.m3u", func(w http.ResponseWriter, r *http.Request) {
			if unhealthy.Load() {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(200)
		})
		mux.HandleFunc("/fail-health", func(w http.ResponseWriter, r *http.Request) { unhealthy.Store(true) })
		mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) { os.Exit(0) })
		fmt.Println("test backend started")
		if err := http.ListenAndServe(net.JoinHostPort("127.0.0.1", port), mux); err != nil {
			os.Exit(2)
		}
		return
	}
	os.Exit(m.Run())
}

func TestBackendConcurrentStartExitAndStop(t *testing.T) {
	t.Setenv("YSP_TEST_BACKEND_PROCESS", "1")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	port, _ := strconv.Atoi(portText)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b := newBackendManager(&http.Client{Timeout: time.Second}, exe, port, t.TempDir())
	defer b.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	readyDone := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-readyDone:
				return
			case <-time.After(time.Millisecond):
				_ = b.Ready()
			}
		}
	}()
	defer func() { close(readyDone); readers.Wait() }()
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- b.Start(ctx) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if !b.Ready() {
		t.Fatal("backend not ready")
	}
	b.mu.Lock()
	originalPID := b.cmd.Process.Pid
	b.mu.Unlock()
	failed, err := b.client.Get(b.endpoint("/fail-health"))
	if err != nil {
		t.Fatal(err)
	}
	failed.Body.Close()
	if err := b.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	restartedPID := b.cmd.Process.Pid
	b.mu.Unlock()
	if originalPID == restartedPID || !b.Ready() {
		t.Fatal("unresponsive child was not restarted")
	}
	resp, _ := b.client.Get(b.endpoint("/exit"))
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	waitFor(t, func() bool { return !b.Ready() })
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	b.Stop()
	if b.Ready() {
		t.Fatal("backend survived Stop")
	}
	if err := b.Start(ctx); err == nil {
		t.Fatal("backend restarted after Stop")
	}
}

func TestBackendFailureCooldownAndRecovery(t *testing.T) {
	var calls atomic.Int32
	var broken atomic.Bool
	broken.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if broken.Load() {
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	m, _ := testManager(t, upstream.URL)
	b := newBackendManager(m.client, "", 8766, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err := b.FetchPlaylist(ctx, "cctv1", "localhost")
	cancel()
	if err == nil {
		t.Fatal("hung backend succeeded")
	}
	for i := 0; i < 5; i++ {
		if _, err := b.FetchPlaylist(context.Background(), "cctv1", "localhost"); err == nil {
			t.Fatal("cooldown bypassed")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("retried unavailable backend %d times", calls.Load())
	}
	broken.Store(false)
	b.mu.Lock()
	b.retryAfter = time.Now().Add(-time.Second)
	b.mu.Unlock()
	if _, err := b.FetchPlaylist(context.Background(), "cctv1", "localhost"); err != nil {
		t.Fatal("backend failed to recover", err)
	}
}
