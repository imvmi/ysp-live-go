package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestJCEPreservesDiscontinuitiesAcrossWindowSlides(t *testing.T) {
	playlists := []string{
		"#EXT-X-DISCONTINUITY-SEQUENCE:10\n#EXTINF:6,\na.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:6,\nb.ts\n#EXTINF:6,\nc.ts\n",
		"#EXT-X-DISCONTINUITY-SEQUENCE:10\n#EXT-X-DISCONTINUITY\n#EXTINF:6,\nb.ts\n#EXTINF:6,\nc.ts\n#EXTINF:6,\nd.ts\n",
		"#EXT-X-DISCONTINUITY-SEQUENCE:11\n#EXTINF:6,\nc.ts\n#EXTINF:6,\nd.ts\n#EXTINF:6,\ne.ts\n",
		"#EXT-X-DISCONTINUITY-SEQUENCE:11\n#EXTINF:6,\ne.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:6,\nf.ts\n#EXTINF:6,\ng.ts\n",
	}
	var step atomic.Int32
	body := &jceWriter{}
	body.integer(0, 0)
	body.str("https://cdn.cctv.cn/jce.m3u8", 2)
	packet, err := wrapJCE(pidTimeShift, body.b, 1, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Upstream-Host") == "jacc.ysp.cctv.cn" {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write(packet)
			return
		}
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n"+playlists[step.Load()])
	}))
	defer upstream.Close()
	m, _ := testManager(t, upstream.URL)
	m.maxSegments = 3
	ch := m.channels["cctv1"]
	wantHeader := []int{10, 10, 11, 11}
	wantMarkers := []int{1, 1, 0, 1}
	wantMediaSequence := []int{1, 2, 3, 5}
	for i := range playlists {
		step.Store(int32(i))
		if !m.refreshOnce(context.Background(), ch) {
			t.Fatal(ch.lastError)
		}
		output := m.buildPlaylist(ch)
		if !strings.Contains(output, fmt.Sprintf("#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", wantHeader[i])) ||
			strings.Count(output, "#EXT-X-DISCONTINUITY\n") != wantMarkers[i] ||
			!strings.Contains(output, fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", wantMediaSequence[i])) {
			t.Fatalf("window %d: %s", i, output)
		}
		// Re-reading the same upstream window must not increment either sequence.
		if !m.refreshOnce(context.Background(), ch) || m.buildPlaylist(ch) != output {
			t.Fatalf("window %d changed on repeat", i)
		}
	}
}

func TestVariantSelectionUsesBandwidthNotOrder(t *testing.T) {
	entries := []string{
		"#EXT-X-STREAM-INF:BANDWIDTH=20000000,CODECS=\"avc1.640028,mp4a.40.2\"\nhigh.m3u8",
		"#EXT-X-STREAM-INF:BANDWIDTH=1000000\nlow.m3u8",
		"#EXT-X-STREAM-INF:BANDWIDTH=4000000\nmedium.m3u8",
	}
	for _, order := range [][]string{entries, {entries[2], entries[0], entries[1]}} {
		for _, tc := range []struct {
			limit int64
			want  string
		}{{defaultMaxBandwidth, "medium.m3u8"}, {2_000_000, "low.m3u8"}, {500_000, "low.m3u8"}, {0, "high.m3u8"}} {
			got, err := selectVariant(strings.Split(strings.Join(order, "\n"), "\n"), tc.limit)
			if err != nil || got != tc.want {
				t.Fatalf("limit %d: %q %v", tc.limit, got, err)
			}
		}
	}
	for _, invalid := range []string{"#EXT-X-STREAM-INF:BANDWIDTH=oops\na.m3u8", "#EXT-X-STREAM-INF:BANDWIDTH=1000", "#EXT-X-STREAM-INF:BANDWIDTH=1000,CODECS=\"unterminated\na.m3u8"} {
		if _, err := selectVariant(strings.Split(invalid, "\n"), defaultMaxBandwidth); err == nil {
			t.Fatalf("accepted invalid variant: %q", invalid)
		}
	}
}

func TestMultiVariantFetchHonorsConfiguredLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/master.m3u8" {
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=20000000\nhigh.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nlow.m3u8\n")
			return
		}
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\n"+strings.TrimPrefix(r.URL.Path, "/")+".ts\n")
	}))
	defer upstream.Close()
	for _, tc := range []struct {
		limit int64
		want  string
	}{{defaultMaxBandwidth, "low.m3u8.ts"}, {0, "high.m3u8.ts"}} {
		playlist, err := fetchAbsolutePlaylist(context.Background(), upstream.Client(), upstream.URL+"/master.m3u8", 0, tc.limit)
		if err != nil || !strings.Contains(playlist, upstream.URL+"/"+tc.want) {
			t.Fatalf("%s %v", playlist, err)
		}
	}
}
