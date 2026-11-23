package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func liveWindow(seq int, name string) string {
	return fmt.Sprintf("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:6,\nhttps://cdn.cctv.cn/%s%d.ts\n#EXTINF:6,\nhttps://cdn.cctv.cn/%s%d.ts\n#EXTINF:6,\nhttps://cdn.cctv.cn/%s%d.ts\n", seq, name, seq, name, seq+1, name, seq+2)
}

func TestTimelineRepeatedProtocolSwitches(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv1"]
	ch.bkPlaylist = liveWindow(10000, "bk")
	ch.lastOK, ch.lastProgress = time.Now(), time.Now()
	previous := mediaPosition(m.buildPlaylist(ch))
	for _, mode := range []string{"jce", "bk", "jce", "bk"} {
		ch.mode = mode
		if mode == "jce" {
			if err := m.mergeSegments(ch, []segment{{Duration: 6, URL: "https://jce.cctv.cn/a.ts"}, {Duration: 6, URL: "https://jce.cctv.cn/b.ts"}}); err != nil {
				t.Fatal(err)
			}
		}
		out := m.buildPlaylist(ch)
		p := mediaPosition(out)
		if p.sequence != previous.sequence+previous.count {
			t.Fatalf("noncontiguous switch %s: %s", mode, out)
		}
		if !strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
			t.Fatal("missing cutover discontinuity")
		}
		if again := m.buildPlaylist(ch); again != out {
			t.Fatal("repeated reads changed timeline")
		}
		previous = p
	}
}

func TestTimelineSlidingDiscontinuitiesAndEncryption(t *testing.T) {
	var timeline playlistTimeline
	timeline.normalize(liveWindow(10000, "bk"), "bk")
	input := strings.Replace(liveWindow(40, "jce"), "#EXTINF:6,", "#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXTINF:6,", 1)
	out := timeline.normalize(input, "jce")
	for _, n := range []int{40, 41, 42} {
		if !strings.Contains(out, fmt.Sprintf("IV=0x%032x", n)) {
			t.Fatal("lost upstream decryption IV", out)
		}
	}
	if timeline.normalize(input, "jce") != out {
		t.Fatal("unstable epoch")
	}
	slid := timeline.normalize(liveWindow(41, "jce"), "jce")
	if mediaPosition(slid).sequence != mediaPosition(out).sequence+1 || mediaPosition(slid).epoch != mediaPosition(out).epoch+1 {
		t.Fatal("sliding cutover lost sequence or epoch", slid)
	}
	if strings.Contains(slid, "#EXT-X-DISCONTINUITY\n") {
		t.Fatal("cutover marker was reinserted")
	}
}

func TestBKRejectsRollbackWithoutPoisoningCache(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv1"]
	m.setBKURLs(ch, []string{"primary", "backup"})
	if err := m.acceptBKSource(ch, playlistResult{url: "primary", playlist: liveWindow(100, "a")}); err != nil {
		t.Fatal(err)
	}
	before := ch.bkPlaylist
	if err := m.acceptBKSource(ch, playlistResult{url: "backup", playlist: liveWindow(90, "b")}); err == nil {
		t.Fatal("accepted rollback")
	}
	if ch.bkPlaylist != before || ch.bkPreferred != "primary" {
		t.Fatal("rejected source mutated cache")
	}
	if err := m.acceptBKSource(ch, playlistResult{url: "backup", playlist: liveWindow(101, "b")}); err != nil {
		t.Fatal("rejected progressing backup", err)
	}
}

func TestEndedLiveSourceFallsBack(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := testPlaylist
		if r.URL.Path == "/ended.m3u8" {
			body += "#EXT-X-ENDLIST\n"
		}
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	m.setBKURLs(ch, []string{"https://cdn.cctv.cn/ended.m3u8", "https://cdn.cctv.cn/live.m3u8"})
	if err := m.ensure(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.buildPlaylist(ch), "#EXT-X-ENDLIST") || !strings.HasSuffix(ch.bkPreferred, "/live.m3u8") {
		t.Fatal("ended source won")
	}
}

func TestFrozenJCEImmediatelyRecoversViaBK(t *testing.T) {
	packet := jceResponsePacket(t, "https://jce-test.cctv.cn/live.m3u8")
	var bk atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Upstream-Host") {
		case "jacc.ysp.cctv.cn":
			_, _ = w.Write(packet)
		case "bkliveinfo.ysp.cctv.cn":
			bk.Add(1)
			_, _ = io.WriteString(w, `{"iretcode":0,"playurl":"https://bk-test.cctv.cn/live.m3u8"}`)
		default:
			_, _ = io.WriteString(w, testPlaylist)
		}
	}))
	defer upstream.Close()
	m, _ := testManager(t, upstream.URL)
	ch := m.channels["cctv1"]
	ch.mode = "jce"
	ch.nextBKProbe = time.Now().Add(time.Hour)
	if !m.refreshOnce(context.Background(), ch) {
		t.Fatal(ch.lastError)
	}
	ch.lastProgress = time.Now().Add(-time.Minute)
	if !m.refreshOnce(context.Background(), ch) || ch.mode != "bk" || bk.Load() != 1 {
		t.Fatalf("no immediate recovery: %s mode=%s calls=%d", ch.lastError, ch.mode, bk.Load())
	}
}

func TestBKRecoveryQueueDoesNotStarveJCE(t *testing.T) {
	packet := jceResponsePacket(t, "https://jce-test.cctv.cn/live.m3u8")
	var jce atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Upstream-Host") == "jacc.ysp.cctv.cn" {
			jce.Add(1)
			_, _ = w.Write(packet)
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	m, _ := testManager(t, upstream.URL)
	ch := m.channels["cctv1"]
	ch.mode = "jce"
	ch.nextBKProbe = time.Now().Add(-time.Second)
	m.bkSlots <- struct{}{}
	defer func() { <-m.bkSlots }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !m.refreshOnce(ctx, ch) || ch.mode != "jce" || jce.Load() != 1 || m.buildPlaylist(ch) == "" {
		t.Fatalf("healthy JCE starved: %s", ch.lastError)
	}
}

func TestAllChannelsColdStartWithProductionRateLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises real 700ms rate limit across all channels")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Upstream-Host") == "bkliveinfo.ysp.cctv.cn" {
			_, _ = io.WriteString(w, `{"iretcode":0,"playurl":"https://bk-test.cctv.cn/live.m3u8"}`)
			return
		}
		_, _ = io.WriteString(w, strings.Replace(testPlaylist, "TARGETDURATION:6", "TARGETDURATION:3600", 1))
	}))
	defer upstream.Close()
	m, _ := testManager(t, upstream.URL)
	m.bkInterval = 700 * time.Millisecond
	app := httptest.NewServer(newAppServer(m, "127.0.0.1", 8767))
	defer app.Close()
	var wg sync.WaitGroup
	for _, spec := range channelSpecs {
		wg.Add(1)
		go func(slug string) {
			defer wg.Done()
			resp, err := http.Get(app.URL + "/" + slug + ".m3u8")
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				t.Errorf("%s: %d %s", slug, resp.StatusCode, body)
			}
		}(spec.Slug)
	}
	wg.Wait()
}
