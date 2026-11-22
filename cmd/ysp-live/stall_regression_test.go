package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEpochResetAfterSlidingWindow(t *testing.T) {
	var timeline playlistTimeline
	withEpoch := func(seq, epoch int) string {
		return strings.Replace(liveWindow(seq, "a"), "#EXTM3U\n", fmt.Sprintf("#EXTM3U\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", epoch), 1)
	}
	timeline.normalize(withEpoch(100, 0), "bk")
	before := timeline.normalize(withEpoch(200, 5), "bk")
	after := timeline.normalize(withEpoch(203, 1), "bk")
	if mediaPosition(after).epoch < mediaPosition(before).epoch || !strings.Contains(after, "#EXT-X-DISCONTINUITY\n") {
		t.Fatalf("epoch reset escaped without new boundary:\n%s", after)
	}
	if timeline.normalize(withEpoch(203, 1), "bk") != after {
		t.Fatal("reset output is unstable")
	}
}

func TestSignedURLRenewalKeepsTimelineAndBoundedHistory(t *testing.T) {
	var timeline playlistTimeline
	for seq := 100; seq < 200; seq++ {
		input := strings.ReplaceAll(liveWindow(seq, "a"), ".ts\n", fmt.Sprintf(".ts?token=%d\n", seq))
		out := timeline.normalize(input, "bk")
		if mediaPosition(out).sequence != uint64(seq) || strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
			t.Fatal("signature renewal incorrectly reset timeline", out)
		}
		if len(timeline.identities) != 3 {
			t.Fatal("timeline history grew beyond current window")
		}
	}
}

func TestCDNChangeDoesNotReuseSequenceForDifferentMedia(t *testing.T) {
	var timeline playlistTimeline
	before := timeline.normalize(liveWindow(100, "a"), "bk")
	input := strings.ReplaceAll(liveWindow(101, "a"), "cdn.cctv.cn", "backup.cctv.cn")
	after := timeline.normalize(input, "bk")
	p, q := mediaPosition(before), mediaPosition(after)
	if q.sequence < p.sequence+p.count || !strings.Contains(after, "#EXT-X-DISCONTINUITY\n") {
		t.Fatal("CDN switch reused published segment numbers", after)
	}
	if timeline.normalize(input, "bk") != after {
		t.Fatal("CDN cutover changes on repeated reads")
	}
}

func TestJCEDoesNotSilentlyDiscardRequiredTags(t *testing.T) {
	for _, tag := range []string{`#EXT-X-KEY:METHOD=AES-128,URI="key.bin"`, `#EXT-X-MAP:URI="init.mp4"`, "#EXT-X-BYTERANGE:100@0"} {
		t.Run(tag, func(t *testing.T) {
			packet := jceResponsePacket(t, "https://cdn.cctv.cn/live.m3u8")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Upstream-Host") == "jacc.ysp.cctv.cn" {
					_, _ = w.Write(packet)
					return
				}
				if r.Header.Get("X-Upstream-Host") == "bkliveinfo.ysp.cctv.cn" {
					_, _ = io.WriteString(w, `{"iretcode":0,"playurl":"https://bk-test.cctv.cn/live.m3u8"}`)
					return
				}
				if r.Header.Get("X-Upstream-Host") == "bk-test.cctv.cn" {
					_, _ = io.WriteString(w, testPlaylist)
					return
				}
				_, _ = io.WriteString(w, strings.Replace(testPlaylist, "#EXTINF:6,", tag+"\n#EXTINF:6,", 1))
			}))
			defer upstream.Close()
			m, _ := testManager(t, upstream.URL)
			ch := m.channels["cctv1"]
			ch.mode = "jce"
			err := m.jceRefresh(context.Background(), ch)
			if !errors.Is(err, errUnsupportedJCE) {
				t.Fatalf("expected explicit format rejection, got %v", err)
			}
			if !m.refreshOnce(context.Background(), ch) || ch.mode != "bk" || !strings.Contains(m.buildPlaylist(ch), "fresh.ts") {
				t.Fatal("unsupported JCE did not recover via BK", ch.lastError)
			}
		})
	}
}

func TestRefreshCadenceIncludesNetworkTime(t *testing.T) {
	starts := make(chan time.Time, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case starts <- time.Now():
		default:
		}
		timer := time.NewTimer(600 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, strings.Replace(testPlaylist, "TARGETDURATION:6", "TARGETDURATION:1", 1))
	}))
	defer upstream.Close()
	m, ch := testManager(t, upstream.URL)
	if err := m.ensure(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	first := <-starts
	select {
	case second := <-starts:
		if elapsed := second.Sub(first); elapsed > 1400*time.Millisecond {
			t.Fatalf("600ms network time added to 1s cadence: %s", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not recur")
	}
}
