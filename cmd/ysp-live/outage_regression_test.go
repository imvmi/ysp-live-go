package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBKRecoversAdvancingSequenceAfterEncoderRestart(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv11"]
	m.setBKURLs(ch, []string{"source"})
	if err := m.acceptBKSource(ch, playlistResult{url: "source", playlist: liveWindow(10000, "old")}); err != nil {
		t.Fatal(err)
	}
	m.finishRefresh(ch, time.Now(), nil)
	before := m.buildPlaylist(ch)
	ch.lastProgress = time.Now().Add(-time.Minute)
	ch.bkLastProgress = ch.lastProgress
	if err := m.acceptBKSource(ch, playlistResult{url: "source", playlist: liveWindow(1, "new")}); err == nil {
		t.Fatal("single rollback sample must not replace cache")
	}
	if err := m.acceptBKSource(ch, playlistResult{url: "source", playlist: liveWindow(2, "new")}); err != nil {
		t.Fatal("advancing restarted encoder remained locked out:", err)
	}
	m.finishRefresh(ch, time.Now(), nil)
	after := m.buildPlaylist(ch)
	if mediaPosition(after).sequence <= mediaPosition(before).sequence || !strings.Contains(after, "#EXT-X-DISCONTINUITY\n") {
		t.Fatal("restart did not preserve output timeline", after)
	}
}

func TestJCEWindowGapEmitsDecoderBoundary(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv1"]
	ch.mode = "jce"
	if err := m.mergeSegments(ch, []segment{{Duration: 6, URL: "https://cdn.cctv.cn/old.ts"}}); err != nil {
		t.Fatal(err)
	}
	m.finishRefresh(ch, time.Now(), nil)
	before := m.buildPlaylist(ch)
	if err := m.mergeSegments(ch, []segment{{Duration: 6, URL: "https://cdn.cctv.cn/new.ts"}}); err != nil {
		t.Fatal(err)
	}
	m.finishRefresh(ch, time.Now(), nil)
	after := m.buildPlaylist(ch)
	if !strings.Contains(after, "#EXT-X-DISCONTINUITY\n") {
		t.Fatal("disjoint JCE windows have no decoder reset", after)
	}
	if mediaPosition(after).sequence != mediaPosition(before).sequence+1 {
		t.Fatal("gap broke local numbering")
	}
	if m.buildPlaylist(ch) != after {
		t.Fatal("gap marker changed on repeat read")
	}
	if err := m.mergeSegments(ch, []segment{{Duration: 6, URL: "https://cdn.cctv.cn/new.ts"}}); err != nil {
		t.Fatal(err)
	}
	if m.buildPlaylist(ch) != after {
		t.Fatal("gap boundary disappeared on identical upstream refresh")
	}
}

func TestJCEGapBoundarySurvivesOutputWindowTrimming(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv1"]
	ch.mode = "jce"
	for _, prefix := range []string{"old", "new"} {
		var items []segment
		for i := 0; i < 20; i++ {
			items = append(items, segment{Duration: 6, URL: fmt.Sprintf("https://cdn.cctv.cn/%s%d.ts", prefix, i)})
		}
		if err := m.mergeSegments(ch, items); err != nil {
			t.Fatal(err)
		}
		m.finishRefresh(ch, time.Now(), nil)
		out := m.buildPlaylist(ch)
		if prefix == "new" && !strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
			t.Fatal("trimmed source boundary was not restored", out)
		}
		if m.buildPlaylist(ch) != out {
			t.Fatal("trimmed boundary is unstable")
		}
	}
}

func TestBKRestartRecoveryStillRejectsStaleCandidates(t *testing.T) {
	for _, scenario := range []string{"healthy-current", "frozen-candidate", "old-wall-clock", "expired-observation"} {
		t.Run(scenario, func(t *testing.T) {
			m := newChannelManager(http.DefaultClient)
			defer m.Close()
			ch := m.channels["cctv11"]
			m.setBKURLs(ch, []string{"source"})
			old, first, next := liveWindow(10000, "old"), liveWindow(1, "new"), liveWindow(2, "new")
			if scenario == "old-wall-clock" {
				stamp := func(s, day string) string {
					return strings.Replace(s, "#EXTINF:6,", "#EXT-X-PROGRAM-DATE-TIME:"+day+"T00:00:00Z\n#EXTINF:6,", 1)
				}
				old = stamp(old, "2026-11-22")
				first = stamp(first, "2026-11-20")
				next = stamp(next, "2026-11-21")
			}
			if err := m.acceptBKSource(ch, playlistResult{url: "source", playlist: old}); err != nil {
				t.Fatal(err)
			}
			m.finishRefresh(ch, time.Now(), nil)
			if scenario != "healthy-current" {
				ch.lastProgress = time.Now().Add(-time.Minute)
				ch.bkLastProgress = ch.lastProgress
			}
			if err := m.acceptBKSource(ch, playlistResult{url: "source", playlist: first}); err == nil {
				t.Fatal("accepted first rollback sample")
			}
			if scenario == "frozen-candidate" {
				next = first
			}
			if scenario == "expired-observation" {
				state := ch.bkSources["source"]
				state.ResetObserved = time.Now().Add(-time.Hour)
				ch.bkSources["source"] = state
			}
			if err := m.acceptBKSource(ch, playlistResult{url: "source", playlist: next}); err == nil {
				t.Fatal("unsafe rollback accepted")
			}
			if ch.bkPlaylist != old {
				t.Fatal("rejected candidate replaced current playlist")
			}
		})
	}
}

func TestBKRestartRecoveryWhileJCEStillAdvances(t *testing.T) {
	m := newChannelManager(http.DefaultClient)
	defer m.Close()
	ch := m.channels["cctv1"]
	m.setBKURLs(ch, []string{"source"})
	if err := m.acceptBKSource(ch, playlistResult{url: "source", playlist: liveWindow(10000, "old")}); err != nil {
		t.Fatal(err)
	}
	ch.mode = "jce"
	ch.lastProgress = time.Now()
	ch.bkLastProgress = time.Now().Add(-time.Minute)
	if err := m.acceptBKSource(ch, playlistResult{url: "source", playlist: liveWindow(1, "new")}); err == nil {
		t.Fatal("reset accepted without confirmation")
	}
	if err := m.acceptBKSource(ch, playlistResult{url: "source", playlist: liveWindow(2, "new")}); err != nil {
		t.Fatal("healthy JCE prevented recovery of restarted BK", err)
	}
}

func TestMixedGapPlaylistRetainsPlayableSegments(t *testing.T) {
	playlist := strings.Replace(liveWindow(1, "media"), "#EXTINF:6,", "#EXT-X-GAP\n#EXTINF:6,", 1)
	if err := validateMediaPlaylist(playlist); err != nil {
		t.Fatal("one gap discarded otherwise playable window", err)
	}
}

func TestAllGapPlaylistDoesNotWinCDNRace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := testPlaylist
		if r.URL.Path == "/gap.m3u8" {
			body = strings.Replace(body, "fresh.ts", "#EXT-X-GAP\nfresh.ts", 1)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	m.setBKURLs(ch, []string{"https://cdn.cctv.cn/gap.m3u8", "https://cdn.cctv.cn/good.m3u8"})
	if err := m.ensure(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	ch.mu.Lock()
	selected := ch.bkPreferred
	ch.mu.Unlock()
	if !strings.HasSuffix(selected, "/good.m3u8") {
		t.Fatal("selected playlist has no downloadable segments", selected)
	}
}
