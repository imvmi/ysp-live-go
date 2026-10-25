package main

import (
	"fmt"
	"io"
)

// Reading one extra byte distinguishes a complete response from a truncated
// playlist whose last (signed) media URL would otherwise look valid.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("upstream response exceeds %d bytes", limit)
	}
	return b, nil
}
