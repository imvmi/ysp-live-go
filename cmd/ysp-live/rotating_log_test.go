package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRotatingLogBoundsDiskAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backend.log")
	for cycle := 0; cycle < 3; cycle++ {
		l, err := newRotatingLog(path, 128, 3)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				data := bytes.Repeat([]byte("x"), 1000)
				if n, err := l.Write(data); err != nil || n != len(data) {
					t.Errorf("write %d: %v", n, err)
				}
			}()
		}
		wg.Wait()
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Write([]byte("closed")); err != os.ErrClosed {
			t.Fatalf("write after close: %v", err)
		}
		for i := 0; i <= 3; i++ {
			name := path
			if i > 0 {
				name = fmt.Sprintf("%s.%d", path, i)
			}
			info, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() > 128 {
				t.Fatalf("unbounded log %s: %d", name, info.Size())
			}
		}
		files, _ := filepath.Glob(path + "*")
		if len(files) != 4 {
			t.Fatalf("%d log files", len(files))
		}
	}
}
