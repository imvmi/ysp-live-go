package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type backendManager struct {
	client *http.Client
	path   string
	port   int
	dir    string

	startMu    sync.Mutex // Startup and watchdog may both call Start.
	mu         sync.Mutex
	cmd        *exec.Cmd
	ready      bool
	logFile    *rotatingLog
	done       chan struct{}
	stopped    bool
	retryAfter time.Time
}

func resolveBackendPath(value, executableDir string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value != "auto" {
		path, err := filepath.Abs(value)
		if err != nil {
			return "", err
		}
		return path, nil
	}
	var names []string
	switch runtime.GOOS {
	case "windows":
		names = []string{"ysptp-project-windows-x64.exe", "ysptp-project.exe"}
	case "linux":
		if runtime.GOARCH == "arm64" {
			names = []string{"ysptp-project-linux-arm64", "ysptp-project-arm64", "ysptp-project"}
		} else {
			names = []string{"ysptp-project-linux-x64", "ysptp-project"}
		}
	case "darwin":
		if runtime.GOARCH == "arm64" {
			names = []string{"ysptp-project-arm64", "ysptp-project"}
		} else {
			names = []string{"ysptp-project-x86_64", "ysptp-project"}
		}
	default:
		names = []string{"ysptp-project"}
	}
	for _, name := range names {
		path := filepath.Join(executableDir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return filepath.Join(executableDir, names[0]), nil
}

func newBackendManager(client *http.Client, path string, port int, dir string) *backendManager {
	return &backendManager{client: client, path: path, port: port, dir: dir}
}

func (b *backendManager) endpoint(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", b.port, path)
}

func (b *backendManager) Ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ready && b.cmd != nil && b.cmd.Process != nil && !time.Now().Before(b.retryAfter)
}

func (b *backendManager) checkHealth(ctx context.Context) error {
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probe, http.MethodHead, b.endpoint("/4kfifa.m3u"), nil)
	if err != nil {
		return err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("backend health HTTP %d", resp.StatusCode)
	}
	return nil
}

func (b *backendManager) Start(ctx context.Context) error {
	b.startMu.Lock()
	defer b.startMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.path == "" {
		return nil
	}
	info, err := os.Stat(b.path)
	if err != nil {
		return fmt.Errorf("4K backend unavailable: %w", err)
	}
	if info.IsDir() {
		return errors.New("4K backend path is a directory")
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		return fmt.Errorf("4K backend is not executable: %s", b.path)
	}

	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return errors.New("4K backend manager stopped")
	}
	if b.cmd != nil {
		cmd, done := b.cmd, b.done
		b.mu.Unlock()
		if err := b.waitUntilReady(ctx); err != nil {
			b.reapFailedStartup(cmd, done)
			return err
		}
		b.mu.Lock()
		b.ready = !b.stopped && b.cmd != nil
		if b.ready {
			b.retryAfter = time.Time{}
		}
		ready := b.ready
		b.mu.Unlock()
		if !ready {
			return errors.New("4K backend exited during startup")
		}
		return nil
	}
	logPath := filepath.Join(b.dir, "ysptp-backend.log")
	logFile, err := newRotatingLog(logPath, 10<<20, 3)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	cmd := exec.Command(
		b.path,
		"--port", fmt.Sprint(b.port),
		"--device-json", filepath.Join(b.dir, "device-state-rs.json"),
		"--meta-json", filepath.Join(b.dir, "proxy-cache-state-rs.json"),
	)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		b.mu.Unlock()
		return err
	}
	done := make(chan struct{})
	b.done = done
	b.cmd = cmd
	b.logFile = logFile
	b.ready = false
	b.mu.Unlock()

	go func() {
		err := cmd.Wait()
		b.mu.Lock()
		if b.cmd == cmd {
			b.ready = false
			b.cmd = nil
			if b.logFile != nil {
				_ = b.logFile.Close()
				b.logFile = nil
			}
		}
		close(done)
		stopped := b.stopped
		b.mu.Unlock()
		if !stopped {
			log.Printf("4K backend exited: %v", err)
		}
	}()

	log.Printf("4K backend starting: %s", b.path)
	if err := b.waitUntilReady(ctx); err != nil {
		b.reapFailedStartup(cmd, done)
		return err
	}
	b.mu.Lock()
	b.ready = !b.stopped && b.cmd == cmd
	if b.ready {
		b.retryAfter = time.Time{}
	}
	ready := b.ready
	b.mu.Unlock()
	if !ready {
		return errors.New("4K backend exited during startup")
	}
	log.Printf("4K backend ready")
	return nil
}

// A startup deadline must not leave an unready child occupying the slot
// forever. Cleanup uses its own bounded wait because startup's context has
// normally already expired. Stop and the process waiter may run concurrently.
func (b *backendManager) reapFailedStartup(cmd *exec.Cmd, done <-chan struct{}) {
	b.mu.Lock()
	ownsUnready := b.cmd == cmd && !b.ready
	b.mu.Unlock()
	if !ownsUnready || cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (b *backendManager) waitUntilReady(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		b.mu.Lock()
		alive := !b.stopped && b.cmd != nil
		b.mu.Unlock()
		if !alive {
			return errors.New("4K backend exited during startup")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, b.endpoint("/4kfifa.m3u"), nil)
		if err == nil {
			resp, doErr := b.client.Do(req)
			if doErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("4K backend startup: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (b *backendManager) Watch(ctx context.Context) {
	if b.path == "" {
		return
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := b.maintain(ctx)
			if err != nil && ctx.Err() == nil {
				log.Printf("4K backend restart failed: %v", err)
			}
		}
	}
}

func (b *backendManager) maintain(ctx context.Context) error {
	b.mu.Lock()
	cmd, done, ready := b.cmd, b.done, b.ready
	b.mu.Unlock()
	if ready {
		if b.checkHealth(ctx) == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.mu.Lock()
		b.ready = false
		b.mu.Unlock()
		// Only terminate the child owned by this manager.
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	startCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	return b.Start(startCtx)
}

func (b *backendManager) Stop() {
	b.mu.Lock()
	b.stopped = true
	b.ready = false
	cmd, done := b.cmd, b.done
	b.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		_ = cmd.Process.Kill()
	}
	// WaitDelay also bounds inherited stdout/stderr pipes after process exit.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

func advertisedHost(hostHeader string) string {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	} else if strings.Count(hostHeader, ":") == 1 {
		host = strings.SplitN(hostHeader, ":", 2)[0]
	}
	host = strings.Trim(host, "[] ")
	if host == "" {
		return ""
	}
	if net.ParseIP(host) != nil {
		return host
	}
	for _, r := range host {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '-' {
			return ""
		}
	}
	return host
}

func (b *backendManager) FetchPlaylist(ctx context.Context, slug, hostHeader string) (result string, resultErr error) {
	b.mu.Lock()
	cooling := time.Now().Before(b.retryAfter)
	b.mu.Unlock()
	if cooling {
		return "", errors.New("4K backend is cooling down")
	}
	defer func() {
		if resultErr != nil && !errors.Is(resultErr, context.Canceled) {
			b.mu.Lock()
			b.retryAfter = time.Now().Add(30 * time.Second)
			b.mu.Unlock()
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.endpoint("/"+slug+".m3u8"), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("4K backend returned HTTP %d", resp.StatusCode)
	}
	body, err := readLimited(resp.Body, 16<<20)
	if err != nil {
		return "", err
	}
	playlist := string(body)
	if !strings.Contains(playlist, "#EXTM3U") {
		return "", errors.New("4K backend returned a non-M3U8 response")
	}
	if err := validateMediaPlaylist(playlist); err != nil {
		return "", err
	}
	host := advertisedHost(hostHeader)
	if host != "" && host != "127.0.0.1" && host != "localhost" {
		from := fmt.Sprintf("http://127.0.0.1:%d/", b.port)
		to := "http://" + net.JoinHostPort(host, fmt.Sprint(b.port)) + "/"
		playlist = strings.ReplaceAll(playlist, from, to)
	}
	return playlist, nil
}
