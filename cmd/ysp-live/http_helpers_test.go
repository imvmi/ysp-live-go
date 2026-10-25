package main

import (
	"strings"
	"testing"
)

func TestReadLimitedRejectsTruncation(t *testing.T) {
	if b, err := readLimited(strings.NewReader("1234"), 4); err != nil || string(b) != "1234" {
		t.Fatalf("exact limit: %s %v", b, err)
	}
	if _, err := readLimited(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("silently truncated response")
	}
}

func TestMediaPlaylistValidation(t *testing.T) {
	if err := validateMediaPlaylist(testPlaylist); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"#EXTM3U\n",
		"#EXTM3U\n#EXTINF:6,\n",
		"#EXTM3U\n#EXTINF:NaN,\nbad.ts\n",
		"#EXTM3U\n#EXTINF:+Inf,\nbad.ts\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nchild.m3u8\n",
	} {
		if err := validateMediaPlaylist(body); err == nil {
			t.Fatalf("accepted invalid playlist: %q", body)
		}
	}
}
