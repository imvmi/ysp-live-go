package main

import (
	"fmt"
	"os"
	"sync"
)

// rotatingLog bounds the external backend's disk use, including across restarts.
// The main application's stderr is managed by the service manager.
type rotatingLog struct {
	mu      sync.Mutex
	path    string
	limit   int64
	backups int
	file    *os.File
	size    int64
}

func newRotatingLog(path string, limit int64, backups int) (*rotatingLog, error) {
	l := &rotatingLog{path: path, limit: limit, backups: backups}
	if limit <= 0 || backups < 1 {
		return nil, fmt.Errorf("invalid log rotation limits")
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *rotatingLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.file, l.size = f, info.Size()
	return nil
}

func (l *rotatingLog) rotate() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	l.file = nil
	for i := l.backups; i >= 1; i-- {
		src := l.path
		if i > 1 {
			src = fmt.Sprintf("%s.%d", l.path, i-1)
		}
		dst := fmt.Sprintf("%s.%d", l.path, i)
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return l.open()
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	written := 0
	for len(p) > 0 {
		if l.file == nil {
			return written, os.ErrClosed
		}
		if l.size >= l.limit {
			if err := l.rotate(); err != nil {
				return written, err
			}
		}
		next := int64(len(p))
		if room := l.limit - l.size; next > room {
			next = room
		}
		n, err := l.file.Write(p[:next])
		written += n
		l.size += int64(n)
		p = p[n:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
