package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServerBKDeviceJCEOverlapRecovery(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv9"]
	ch.workerRunning = true // Drive source availability without a background fetch.
	ch.mode = "bk"
	ch.bkPlaylist = overlapWindow(100, 10, 3, "bk")
	ch.lastOK, ch.lastProgress = time.Now(), time.Now()
	app := newAppServer(m, "127.0.0.1", 8767)
	calls := 0
	deviceText := overlapWindow(900, 10, 1, "device-go")
	app.device = &stubDeviceSource{fetch: func(context.Context, string, string) (string, error) {
		calls++
		return deviceText, nil
	}}
	get := func(want uint64) string {
		t.Helper()
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", "/cctv9.m3u8", nil))
		if w.Code != http.StatusOK || mediaPosition(w.Body.String()).sequence != want || strings.Contains(w.Body.String(), "#EXT-X-DISCONTINUITY\n") {
			t.Fatalf("status=%d want sequence=%d body=%s", w.Code, want, w.Body.String())
		}
		return w.Header().Get("X-YSP-Source")
	}
	get(100)
	ch.bkPlaylist = overlapWindow(101, 11, 3, "bk")
	get(101)
	ch.lastOK, ch.lastProgress = time.Now().Add(-time.Minute), time.Now().Add(-time.Minute)
	ch.nextRetry = time.Now().Add(time.Minute)
	ch.lastError = "primary source unavailable"
	if source := get(101); source != "bk" {
		t.Fatalf("held playlist reported wrong source: %q", source)
	}
	deviceText = overlapWindow(902, 12, 3, "device-go")
	if source := get(102); source != "device-go" {
		t.Fatalf("caught-up device reported wrong source: %q", source)
	}
	ch.mode = "jce"
	var segments []segment
	for i := 13; i < 16; i++ {
		segments = append(segments, segment{Duration: 6,
			PDT: time.Unix(1700000000+int64(i*6), 0).UTC().Format(time.RFC3339),
			URL: fmt.Sprintf("https://jce.ysp.cctv.cn/new-signature/2024078603-%d.ts", 1000+i)})
	}
	if err := m.mergeSegments(ch, segments); err != nil {
		t.Fatal(err)
	}
	ch.lastOK, ch.lastProgress = time.Now(), time.Now()
	ch.lastError = ""
	get(103)
	get(103)
	if calls != 2 {
		t.Fatalf("healthy primary still fetched device: %d", calls)
	}
}

func overlapWindow(sequence, first, count int, source string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:%d\n", sequence)
	for i := first; i < first+count; i++ {
		if source == "bk" {
			fmt.Fprintf(&b, "#EXT-SID:%d\n", 1000+i)
		}
		stamp := time.Unix(1700000000+int64(i*6), 0).UTC().Format(time.RFC3339)
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n#EXTINF:6.000,\nhttps://%s.ysp.cctv.cn/signature-%d/2024078603-%d.ts\n", stamp, source, sequence, 1000+i)
	}
	return b.String()
}

func TestTimelineCrossSourceOverlap(t *testing.T) {
	device := newDeviceManager(http.DefaultClient, "")
	defer device.Close()
	for _, from := range []string{"bk", "jce", "device-go"} {
		for _, to := range []string{"bk", "jce", "device-go"} {
			if from == to {
				continue
			}
			t.Run(from+"-to-"+to, func(t *testing.T) {
				var timeline playlistTimeline
				render := func(source, text string) string {
					if source == "device-go" {
						text = rewriteDevicePlaylist(text, "https://device-go.ysp.cctv.cn/live.m3u8", "localhost:8767", "test-entry", device.signMediaURL)
						return timeline.normalizeWithIdentity(text, source, device.MediaIdentity)
					}
					return timeline.normalize(text, source)
				}
				render(from, overlapWindow(100, 10, 3, from))
				input := overlapWindow(900, 11, 3, to)
				input = strings.Replace(input, "#EXTM3U\n", "#EXTM3U\n#EXT-X-DISCONTINUITY-SEQUENCE:7\n", 1)
				out := render(to, input)
				if p := mediaPosition(out); p.sequence != 101 || p.count != 3 || p.epoch != 0 || strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
					t.Fatalf("overlap acquired new sequence/epoch: %s", out)
				}
				if render(to, input) != out {
					t.Fatal("repeated response changed")
				}
				out = render(to, strings.Replace(overlapWindow(901, 12, 3, to), "#EXTM3U\n", "#EXTM3U\n#EXT-X-DISCONTINUITY-SEQUENCE:7\n", 1))
				if mediaPosition(out).sequence != 102 || strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
					t.Fatalf("sliding window lost alignment: %s", out)
				}
			})
		}
	}
}

func TestTimelineOverlapTrimsUnverifiedOlderPrefix(t *testing.T) {
	var timeline playlistTimeline
	timeline.normalize(overlapWindow(100, 10, 3, "bk"), "bk")
	input := overlapWindow(900, 8, 6, "jce")
	// A boundary in the discarded prefix must not leak into retained media.
	input = strings.Replace(input, "#EXTINF:", "#EXT-X-DISCONTINUITY\n#EXTINF:", 1)
	out := timeline.normalize(input, "jce")
	if p := mediaPosition(out); p.sequence != 100 || p.count != 4 || p.epoch != 0 || strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
		t.Fatalf("older prefix changed published timeline: %s", out)
	}
	if strings.Contains(out, "-1008.ts") || strings.Contains(out, "-1009.ts") || strings.Count(out, "#EXT-X-PROGRAM-DATE-TIME:") != 4 {
		t.Fatalf("discarded prefix metadata leaked: %s", out)
	}
	if timeline.normalize(input, "jce") != out {
		t.Fatal("trimmed prefix returned on repeat read")
	}
	// After the discarded discontinuity slides out, its epoch moves to the header.
	slid := strings.Replace(overlapWindow(901, 9, 6, "jce"), "#EXTM3U\n", "#EXTM3U\n#EXT-X-DISCONTINUITY-SEQUENCE:1\n", 1)
	out = timeline.normalize(slid, "jce")
	if mediaPosition(out).sequence != 100 || strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
		t.Fatalf("discarded boundary changed sliding output: %s", out)
	}
	var plain playlistTimeline
	plain.normalize(overlapWindow(100, 10, 3, "bk"), "bk")
	plain.normalize(overlapWindow(900, 8, 6, "jce"), "jce")
	for start := 9; start <= 12; start++ {
		out = plain.normalize(overlapWindow(900+start-8, start, 6, "jce"), "jce")
		want := 100
		if start > 10 {
			want += start - 10
		}
		if mediaPosition(out).sequence != uint64(want) || strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
			t.Fatalf("sliding trimmed window changed alignment: %s", out)
		}
	}
}

func TestTimelineOverlapLagDoesNotLowerHighWaterMark(t *testing.T) {
	var timeline playlistTimeline
	timeline.normalize(overlapWindow(100, 10, 3, "bk"), "bk")
	out := timeline.normalize(overlapWindow(900, 10, 2, "jce"), "jce")
	if mediaPosition(out).sequence != 100 || timeline.last != 102 {
		t.Fatalf("lagging window changed published end: %s last=%d", out, timeline.last)
	}
	out = timeline.normalize(overlapWindow(50, 11, 3, "device-go"), "device-go")
	if mediaPosition(out).sequence != 101 || timeline.last != 103 {
		t.Fatalf("lagging source lost retained overlap: %s", out)
	}
}

func TestTimelineLaggingSourceCannotMoveWindowBackwards(t *testing.T) {
	var timeline playlistTimeline
	timeline.normalize(overlapWindow(100, 10, 3, "bk"), "bk")
	latest := timeline.normalize(overlapWindow(101, 11, 3, "bk"), "bk")
	old := overlapWindow(900, 10, 1, "jce")
	for i := 0; i < 2; i++ {
		if got := timeline.normalize(old, "jce"); got != latest || timeline.source != "bk" {
			t.Fatalf("entirely older source replaced live window: %s", got)
		}
	}
	input := overlapWindow(900, 10, 5, "jce")
	out := timeline.normalize(input, "jce")
	if p := mediaPosition(out); p.sequence != 101 || p.count != 4 || strings.Contains(out, "-1010.ts") {
		t.Fatalf("lagging prefix moved output backwards: %s", out)
	}
	if timeline.normalize(input, "jce") != out {
		t.Fatal("older prefix returned on repeated read")
	}
}

func TestTimelineCrossSourceCannotAlignDifferentMedia(t *testing.T) {
	for name, change := range map[string]func(string) string{
		"different rendition": func(s string) string { return strings.ReplaceAll(s, "2024078603-", "2024078604-") },
		"different duration":  func(s string) string { return strings.ReplaceAll(s, "6.000,", "5.000,") },
		"different timestamp": func(s string) string { return strings.ReplaceAll(s, "2023-11-14", "2023-11-15") },
		"missing timestamp":   func(s string) string { return strings.ReplaceAll(s, "#EXT-X-PROGRAM-DATE-TIME:", "#IGNORED:") },
		"conflicting overlap": func(s string) string { return strings.Replace(s, "2024078603-1012.ts", "2024078603-9999.ts", 1) },
		"range":               func(s string) string { return strings.ReplaceAll(s, "#EXTINF:", "#EXT-X-BYTERANGE:100@0\n#EXTINF:") },
		"map": func(s string) string {
			return strings.Replace(s, "#EXTM3U\n", "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n", 1)
		},
		"encryption": func(s string) string {
			return strings.Replace(s, "#EXTM3U\n", "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n", 1)
		},
		"boundary mismatch": func(s string) string {
			return strings.Replace(s, "#EXTINF:6.000,\nhttps://jce.ysp.cctv.cn/signature-900/2024078603-1012.ts", "#EXT-X-DISCONTINUITY\n#EXTINF:6.000,\nhttps://jce.ysp.cctv.cn/signature-900/2024078603-1012.ts", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var timeline playlistTimeline
			timeline.normalize(overlapWindow(100, 10, 3, "bk"), "bk")
			input := change(overlapWindow(900, 11, 3, "jce"))
			out := timeline.normalize(input, "jce")
			if mediaPosition(out).sequence != 103 || !strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
				t.Fatalf("unverified media reused old sequence: %s", out)
			}
			if name == "encryption" && !strings.Contains(out, fmt.Sprintf("IV=0x%032x", 900)) {
				t.Fatal("fallback lost original encryption IV")
			}
		})
	}
}

func TestTimelineRepeatedAlignedSwitchesBoundHistory(t *testing.T) {
	var timeline playlistTimeline
	for i := 0; i < 200; i++ {
		source := []string{"bk", "jce", "device-go"}[i%3]
		input := overlapWindow(1000+i*10, i, 3, source)
		out := timeline.normalize(input, source)
		if mediaPosition(out).sequence != uint64(1000+i) || strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
			t.Fatalf("switch %d reset timeline: %s", i, out)
		}
		if len(timeline.retained) > 64 || len(timeline.identities) != 3 {
			t.Fatal("unbounded timeline history")
		}
	}
}

func TestTimelineOverlapPreservesExistingDecoderBoundary(t *testing.T) {
	var timeline playlistTimeline
	before := overlapWindow(100, 10, 3, "bk")
	before = strings.Replace(before, "#EXT-SID:1011\n", "#EXT-X-DISCONTINUITY\n#EXT-SID:1011\n", 1)
	timeline.normalize(before, "bk")
	input := overlapWindow(900, 10, 4, "jce")
	stamp := time.Unix(1700000000+11*6, 0).UTC().Format(time.RFC3339)
	input = strings.Replace(input, "#EXT-X-PROGRAM-DATE-TIME:"+stamp, "#EXT-X-DISCONTINUITY\n#EXT-X-PROGRAM-DATE-TIME:"+stamp, 1)
	out := timeline.normalize(input, "jce")
	if p := mediaPosition(out); p.sequence != 100 || p.epoch != 0 || strings.Count(out, "#EXT-X-DISCONTINUITY\n") != 1 {
		t.Fatalf("existing boundary was lost or duplicated: %s", out)
	}
	slid := strings.Replace(overlapWindow(902, 12, 3, "jce"), "#EXTM3U\n", "#EXTM3U\n#EXT-X-DISCONTINUITY-SEQUENCE:1\n", 1)
	out = timeline.normalize(slid, "jce")
	if p := mediaPosition(out); p.sequence != 102 || p.epoch != 1 || strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
		t.Fatalf("sliding boundary lost epoch alignment: %s", out)
	}
}

func TestTimelineOverlapExactURLAndAmbiguousIdentity(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		var timeline playlistTimeline
		timeline.normalize(liveWindow(100, "media"), "bk")
		input := strings.Replace(liveWindow(101, "media"), "MEDIA-SEQUENCE:101", "MEDIA-SEQUENCE:900", 1)
		if duplicate {
			input = strings.Replace(input, "media102.ts", "media101.ts", 1)
		}
		out := timeline.normalize(input, "jce")
		want := uint64(101)
		if duplicate {
			want = 103
		}
		if mediaPosition(out).sequence != want || strings.Contains(out, "#EXT-X-DISCONTINUITY\n") != duplicate {
			t.Fatalf("duplicate=%t got %s", duplicate, out)
		}
	}
}
