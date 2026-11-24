package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Sanitized metadata from the CCTV-9 capture on 2026-11-24. The two CDNs
// returned byte-identical media for SID 1793115687 (SHA-256 638d3007...474b38e).
func capturedBKWindow(start int, host, signature string) string {
	times := []string{"19:00:42", "19:00:47", "19:00:52", "19:00:57"}
	durations := []string{"5.400", "5.000", "4.720", "7.120"}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-TARGETDURATION:8\n#EXT-X-MEDIA-SEQUENCE:%d\n", 1793115686+start)
	for i := start; i < start+3; i++ {
		sid := 1793115686 + i
		fmt.Fprintf(&b, "#EXT-SID:%d\n#EXT-X-PROGRAM-DATE-TIME:2026-11-24T%s+08:00\n#EXTINF:%s,\nhttps://%s/%s/2024078603-%d.ts\n", sid, times[i], durations[i], host, signature, sid)
	}
	return b.String()
}

func TestBKCDNCutoverPreservesPublishedSegments(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv9"]
	ch.mode = "bk"
	ch.lastOK, ch.lastProgress = time.Now(), time.Now()
	ch.bkPlaylist = capturedBKWindow(0, "hlslive-tx-cdn.ysp.cctv.cn", "old-signature")
	before := mediaPosition(m.buildPlaylist(ch))
	for i := 0; i < 20; i++ {
		host := "bktlivecloud-cdn.ysp.cctv.cn"
		if i%4 >= 2 {
			host = "hlslive-tx-cdn.ysp.cctv.cn"
		}
		ch.bkPlaylist = capturedBKWindow(1, host, fmt.Sprintf("renewed-%d", i))
		out := m.buildPlaylist(ch)
		if p := mediaPosition(out); p.sequence != before.sequence+1 || p.epoch != before.epoch || p.count != 3 {
			t.Fatalf("CDN cutover republished retained media: before=%+v after=%+v\n%s", before, p, out)
		}
		if strings.Contains(out, "#EXT-X-DISCONTINUITY\n") || !strings.Contains(out, host+fmt.Sprintf("/renewed-%d/", i)) {
			t.Fatalf("unexpected boundary or stale URL: %s", out)
		}
		if again := m.buildPlaylist(ch); again != out {
			t.Fatal("repeated read changed timeline")
		}
		if len(ch.output.identities) != 3 {
			t.Fatal("history is not bounded by current window")
		}
	}
}

func TestBKCDNIdentityRequiresMatchingMediaMetadata(t *testing.T) {
	for name, change := range map[string]func(string) string{
		"different stream":     func(s string) string { return strings.ReplaceAll(s, "2024078603-", "2024078604-") },
		"different time":       func(s string) string { return strings.ReplaceAll(s, "2026-11-24", "2026-11-25") },
		"different duration":   func(s string) string { return strings.ReplaceAll(s, "5.000,", "4.000,") },
		"different byte range": func(s string) string { return strings.ReplaceAll(s, "#EXTINF:", "#EXT-X-BYTERANGE:100@0\n#EXTINF:") },
		"missing sid":          func(s string) string { return strings.ReplaceAll(s, "#EXT-SID:", "#IGNORED:") },
		"invalid sid":          func(s string) string { return strings.ReplaceAll(s, "#EXT-SID:", "#EXT-SID:invalid") },
		"mismatched sid":       func(s string) string { return strings.Replace(s, "#EXT-SID:1793115687", "#EXT-SID:1793115686", 1) },
		"one missing sid":      func(s string) string { return strings.Replace(s, "#EXT-SID:1793115688\n", "", 1) },
		"missing time":         func(s string) string { return strings.ReplaceAll(s, "#EXT-X-PROGRAM-DATE-TIME:", "#IGNORED:") },
		"invalid time":         func(s string) string { return strings.ReplaceAll(s, "2026-11-24T", "invalidT") },
		"unrecognized host":    func(s string) string { return strings.ReplaceAll(s, "bktlivecloud-cdn.ysp.cctv.cn", "cdn.example") },
		"encryption": func(s string) string {
			return strings.Replace(s, "#EXTM3U\n", "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n", 1)
		},
		"initialization section": func(s string) string {
			return strings.Replace(s, "#EXTM3U\n", "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var timeline playlistTimeline
			before := mediaPosition(timeline.normalize(capturedBKWindow(0, "hlslive-tx-cdn.ysp.cctv.cn", "old"), "bk"))
			after := timeline.normalize(change(capturedBKWindow(1, "bktlivecloud-cdn.ysp.cctv.cn", "new")), "bk")
			if mediaPosition(after).sequence != before.sequence+before.count || !strings.Contains(after, "#EXT-X-DISCONTINUITY\n") {
				t.Fatalf("different or unverifiable media reused sequence: %s", after)
			}
		})
	}
}
