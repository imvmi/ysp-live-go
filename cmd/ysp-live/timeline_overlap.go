package main

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

type timelineMedia struct {
	identity string
	sequence uint64
	epoch    uint64
}

func mediaSID(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	_, sid, _ := strings.Cut(strings.TrimSuffix(path.Base(u.Path), ".ts"), "-")
	return sid
}

// Cross-source matching is deliberately limited to complete, unencrypted
// segments. Identical wall-clock times alone do not establish media identity.
func overlapMedia(text string, mediaIdentity func(string) string) []timelineMedia {
	for _, tag := range []string{"#EXT-X-KEY:", "#EXT-X-MAP:", "#EXT-X-BYTERANGE:", "#EXT-X-GAP", "#EXT-X-I-FRAMES-ONLY"} {
		if strings.Contains(text, tag) {
			return nil
		}
	}
	p := mediaPosition(text)
	seq, epoch := p.sequence, p.epoch
	var sid, pdt, duration string
	var items []timelineMedia
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "#EXT-X-DISCONTINUITY":
			epoch++
		case strings.HasPrefix(line, "#EXT-SID:"):
			sid = strings.TrimPrefix(line, "#EXT-SID:")
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			pdt = strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:")
		case strings.HasPrefix(line, "#EXTINF:"):
			duration = strings.SplitN(strings.TrimPrefix(line, "#EXTINF:"), ",", 2)[0]
		case line != "" && !strings.HasPrefix(line, "#"):
			uri := mediaIdentity(line)
			key := ""
			length, err := time.ParseDuration(duration + "s")
			if err == nil && length > 0 {
				key = uri + "|" + length.String()
				if pdt != "" {
					stamp, err := time.Parse(time.RFC3339Nano, pdt)
					if err != nil {
						key = ""
					} else {
						key += "|" + stamp.UTC().Format(time.RFC3339Nano)
					}
				}
				// JCE drops EXT-SID when rebuilding its playlist, and device
				// relays wrap the URL. The verified original filename still
				// carries the provider's stream and segment identifiers.
				if sid == "" {
					sid = mediaSID(uri)
				}
				if stable, ok := bkMediaIdentity(uri, sid, pdt, duration); ok {
					key = stable
				}
			}
			items = append(items, timelineMedia{key, seq, epoch})
			seq++
			sid, pdt, duration = "", "", ""
		}
	}
	return items
}

// Require one unambiguous contiguous overlap, including matching decoder
// boundaries. Everything before the first verified segment will be trimmed.
// A skip equal to len(next) means the whole verified window is too old.
func matchingOverlap(old, next []timelineMedia, minimumSequence uint64) (int, timelineMedia, bool) {
	positions := make(map[string]int, len(old))
	for i, item := range old {
		if item.identity == "" {
			continue
		}
		if _, exists := positions[item.identity]; exists {
			return 0, timelineMedia{}, false
		}
		positions[item.identity] = i
	}
	seen := make(map[string]bool, len(next))
	firstOld, firstNew := -1, -1
	for j, item := range next {
		if item.identity == "" {
			continue
		}
		if seen[item.identity] {
			return 0, timelineMedia{}, false
		}
		seen[item.identity] = true
		if i, ok := positions[item.identity]; ok {
			if firstOld < 0 {
				firstOld, firstNew = i, j
			} else if i-firstOld != j-firstNew {
				return 0, timelineMedia{}, false
			}
		}
	}
	if firstOld < 0 {
		return 0, timelineMedia{}, false
	}
	for i, j := firstOld, firstNew; i < len(old) && j < len(next); i, j = i+1, j+1 {
		if old[i].identity == "" || old[i].identity != next[j].identity ||
			old[i].epoch-old[firstOld].epoch != next[j].epoch-next[firstNew].epoch {
			return 0, timelineMedia{}, false
		}
	}
	for old[firstOld].sequence < minimumSequence {
		firstOld++
		firstNew++
		if firstNew == len(next) {
			return firstNew, timelineMedia{}, true
		}
	}
	return firstNew, old[firstOld], true
}

// Only used for the simple TS playlists accepted by overlapMedia. Keep global
// tags, discard skipped segments together with their PDT/SID/discontinuities.
func trimTimelinePrefix(text string, count int) string {
	if count == 0 {
		return text
	}
	p := mediaPosition(text)
	var header, body []string
	skipped := 0
	epoch := p.epoch
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		tag := strings.SplitN(line, ":", 2)[0]
		switch tag {
		case "", "#EXTM3U", "#EXT-X-MEDIA-SEQUENCE", "#EXT-X-DISCONTINUITY-SEQUENCE", "#EXT-X-START":
			continue
		case "#EXT-X-VERSION", "#EXT-X-TARGETDURATION", "#EXT-X-ALLOW-CACHE", "#EXT-X-PLAYLIST-TYPE", "#EXT-X-INDEPENDENT-SEGMENTS":
			header = append(header, line)
			continue
		}
		if skipped >= count {
			body = append(body, line)
			continue
		}
		if line == "#EXT-X-DISCONTINUITY" {
			epoch++
		}
		if !strings.HasPrefix(line, "#") {
			skipped++
		}
	}
	return fmt.Sprintf("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", p.sequence+uint64(count), epoch) +
		strings.Join(append(header, body...), "\n") + "\n"
}

// Retain a bounded suffix so a temporarily shorter/lagging source cannot make
// already published media acquire new sequence numbers on the next switch.
func retainTimelineMedia(old, next []timelineMedia) []timelineMedia {
	if len(next) == 0 {
		return nil
	}
	var result []timelineMedia
	for _, item := range old {
		if item.sequence < next[0].sequence {
			result = append(result, item)
		}
	}
	result = append(result, next...)
	for _, item := range old {
		if item.sequence > next[len(next)-1].sequence {
			result = append(result, item)
		}
	}
	if len(result) > 64 {
		result = result[len(result)-64:]
	}
	// A fallback can leave a sequence gap. Older history across that gap
	// cannot be used as a contiguous alignment anchor.
	for i := len(result) - 1; i > 0; i-- {
		if result[i].sequence != result[i-1].sequence+1 {
			return result[i:]
		}
	}
	return result
}
