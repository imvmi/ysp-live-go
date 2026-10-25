package main

import (
	"errors"
	"strconv"
	"strings"
)

const defaultMaxBandwidth int64 = 6_000_000

// Attribute values such as CODECS and GROUP-ID can contain quoted commas.
func playlistAttributes(raw string) (map[string]string, error) {
	attrs := make(map[string]string)
	quoted, start := false, 0
	for i := 0; i <= len(raw); i++ {
		if i < len(raw) && raw[i] == '"' {
			quoted = !quoted
		}
		if i < len(raw) && (raw[i] != ',' || quoted) {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(raw[start:i]), "=")
		if !ok || key == "" || quoted {
			return nil, errors.New("invalid HLS attribute list")
		}
		attrs[key] = strings.Trim(value, `"`)
		start = i + 1
	}
	return attrs, nil
}

// Select the highest peak bandwidth within the configured limit, or the
// lowest available bandwidth if all exceed it. Zero explicitly means highest.
// The same inputs choose the same variant regardless of upstream ordering.
func selectVariant(lines []string, limit int64) (string, error) {
	type variant struct {
		uri       string
		bandwidth int64
	}
	var best, lowest variant
	for i, line := range lines {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "#EXT-X-STREAM-INF:")
		if !ok {
			continue
		}
		attrs, err := playlistAttributes(value)
		if err != nil {
			return "", err
		}
		bandwidth, err := strconv.ParseInt(attrs["BANDWIDTH"], 10, 64)
		if err != nil || bandwidth <= 0 {
			return "", errors.New("invalid variant bandwidth")
		}
		uri := ""
		for j := i + 1; j < len(lines); j++ {
			s := strings.TrimSpace(lines[j])
			if strings.HasPrefix(s, "#EXT-X-STREAM-INF:") {
				break
			}
			if s != "" && !strings.HasPrefix(s, "#") {
				uri = s
				break
			}
		}
		if uri == "" {
			return "", errors.New("variant has no URI")
		}
		v := variant{uri, bandwidth}
		if lowest.uri == "" || bandwidth < lowest.bandwidth || (bandwidth == lowest.bandwidth && uri < lowest.uri) {
			lowest = v
		}
		if (limit == 0 || bandwidth <= limit) && (best.uri == "" || bandwidth > best.bandwidth || (bandwidth == best.bandwidth && uri < best.uri)) {
			best = v
		}
	}
	if best.uri != "" {
		return best.uri, nil
	}
	if lowest.uri != "" {
		return lowest.uri, nil
	}
	return "", errors.New("master playlist contains no variants")
}
