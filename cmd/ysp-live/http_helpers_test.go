package main

import (
	"os"
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

func TestEnvironmentPort(t *testing.T) {
	original, present := os.LookupEnv("PORT")
	t.Cleanup(func() {
		if present {
			_ = os.Setenv("PORT", original)
		} else {
			_ = os.Unsetenv("PORT")
		}
	})
	for _, tc := range []struct {
		value string
		want  int
		ok    bool
	}{
		{"", 8767, true},
		{"8080", 8080, true},
		{"0", 0, false},
		{"bad", 0, false},
	} {
		_ = os.Setenv("PORT", tc.value)
		got, err := environmentPort()
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("PORT=%q: got (%d, %v), want (%d, ok=%t)", tc.value, got, err, tc.want, tc.ok)
		}
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
