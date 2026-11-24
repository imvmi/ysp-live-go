package main

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Playlist sequence numbers belong to a source, not to a channel. Keep one
// client-visible sequence space even when the upstream protocol changes.
// Access is serialized by channel.mu.
type playlistTimeline struct {
	source                string
	inputBase, outputBase uint64
	epochIn, epochOut     uint64
	last, lastEpoch       uint64
	boundary              uint64
	lastStart             uint64
	lastInputEpoch        uint64
	identities            map[uint64]string
	switched              bool
	retained              []timelineMedia
	trimOrigin, trimFloor uint64
	lastOutputStart       uint64
	lastPlaylist          string
}

type playlistPosition struct {
	sequence, epoch, count uint64
	lastTime               time.Time
}

func mediaPosition(text string) playlistPosition {
	var p playlistPosition
	var clock time.Time
	var duration float64
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			p.sequence, _ = strconv.ParseUint(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			p.epoch, _ = strconv.ParseUint(strings.TrimPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"), 10, 64)
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			clock, _ = time.Parse(time.RFC3339Nano, strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"))
		case strings.HasPrefix(line, "#EXTINF:"):
			duration, _ = strconv.ParseFloat(strings.SplitN(strings.TrimPrefix(line, "#EXTINF:"), ",", 2)[0], 64)
		case line != "" && !strings.HasPrefix(line, "#"):
			p.count++
			if !clock.IsZero() {
				clock = clock.Add(time.Duration(duration * float64(time.Second)))
				p.lastTime = clock
			}
		}
	}
	return p
}

func playlistMovedBackwards(old, next string) bool {
	a, b := mediaPosition(old), mediaPosition(next)
	if a.count == 0 || b.count == 0 {
		return false
	}
	if !a.lastTime.IsZero() && !b.lastTime.IsZero() {
		return b.lastTime.Before(a.lastTime)
	}
	return b.sequence+b.count < a.sequence+a.count || b.sequence < a.sequence
}

func (t *playlistTimeline) normalize(text, source string) string {
	return t.normalizeWithIdentity(text, source, func(uri string) string { return segmentKey(uri, "") })
}

// BK serves the same TS file through multiple CDNs and signed path prefixes.
// Only use the provider's stable identity when all identifying metadata is
// present; a basename alone cannot distinguish unrelated streams or restarts.
func bkMediaIdentity(rawURL, sid, pdt, duration string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".ysp.cctv.cn") {
		return "", false
	}
	name := path.Base(u.Path)
	stream, fileSID, ok := strings.Cut(strings.TrimSuffix(name, ".ts"), "-")
	if !ok || !strings.HasSuffix(name, ".ts") || fileSID != sid {
		return "", false
	}
	if _, err := strconv.ParseUint(stream, 10, 64); err != nil {
		return "", false
	}
	if _, err := strconv.ParseUint(sid, 10, 64); err != nil {
		return "", false
	}
	stamp, err := time.Parse(time.RFC3339Nano, pdt)
	if err != nil {
		return "", false
	}
	length, err := time.ParseDuration(duration + "s")
	if err != nil || length <= 0 {
		return "", false
	}
	return "bk:" + name + "|" + stamp.UTC().Format(time.RFC3339Nano) + "|" + length.String(), true
}

func (t *playlistTimeline) normalizeWithIdentity(text, source string, mediaIdentity func(string) string) string {
	p := mediaPosition(text)
	if p.count == 0 {
		return ""
	}
	if source == t.source && p.sequence >= t.trimOrigin && p.sequence < t.trimFloor && t.trimFloor-p.sequence < p.count && len(overlapMedia(text, mediaIdentity)) > 0 {
		text = trimTimelinePrefix(text, int(t.trimFloor-p.sequence))
		p = mediaPosition(text)
	}
	var anchor timelineMedia
	aligned := false
	origin := p.sequence
	if t.source != "" && source != t.source {
		items := overlapMedia(text, mediaIdentity)
		if skip, match, ok := matchingOverlap(t.retained, items, t.lastOutputStart); ok {
			if skip == len(items) {
				// Every incoming segment is already behind the published
				// window. Do not move MEDIA-SEQUENCE backwards or replay it.
				return t.lastPlaylist
			}
			trimmed := trimTimelinePrefix(text, skip)
			position := mediaPosition(trimmed)
			// A leading upstream discontinuity must fit below the retained
			// segment's epoch; otherwise keep the explicit fallback boundary.
			if leading := items[skip].epoch - position.epoch; match.epoch >= leading {
				text, p, anchor, aligned = trimmed, position, match, true
				anchor.epoch -= leading
			}
		}
	}
	identities := make(map[uint64]string, p.count)
	inputIndex := p.sequence
	byteRange := ""
	var sid, pdt, duration string
	// Encrypted media and initialization sections need additional identity
	// rules. Keep the existing URL-based behavior for these formats.
	complexMedia := strings.Contains(text, "#EXT-X-KEY:") || strings.Contains(text, "#EXT-X-MAP:")
	conflict := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			byteRange = line
		case strings.HasPrefix(line, "#EXT-SID:"):
			sid = strings.TrimPrefix(line, "#EXT-SID:")
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			pdt = strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:")
		case strings.HasPrefix(line, "#EXTINF:"):
			duration = strings.SplitN(strings.TrimPrefix(line, "#EXTINF:"), ",", 2)[0]
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		identity := mediaIdentity(line)
		if !complexMedia {
			if sid == "" && (source == "jce" || source == "device-go") {
				sid = mediaSID(identity)
			}
			if stable, ok := bkMediaIdentity(identity, sid, pdt, duration); ok {
				identity = stable
			}
		}
		identity += "|" + byteRange
		identities[inputIndex] = identity
		if old, ok := t.identities[inputIndex]; ok && old != identity {
			conflict = true
		}
		inputIndex++
		byteRange = ""
		sid, pdt, duration = "", "", ""
	}
	if aligned {
		t.source, t.inputBase, t.outputBase = source, p.sequence, anchor.sequence
		t.epochIn, t.epochOut = p.epoch, anchor.epoch
		t.switched = false
		t.trimOrigin, t.trimFloor = origin, p.sequence
	} else if t.source == "" {
		t.source, t.inputBase, t.outputBase = source, p.sequence, p.sequence
		t.epochIn, t.epochOut = p.epoch, p.epoch
	} else if t.source != source || p.sequence < t.lastStart || p.epoch < t.lastInputEpoch || conflict ||
		(p.epoch > t.lastInputEpoch && p.sequence >= t.lastStart+uint64(len(t.identities))) {
		// A discontinuity can already have slid out of a disjoint new
		// window. Emit a visible boundary for viewers of the old window.
		t.source, t.inputBase, t.outputBase = source, p.sequence, t.last+1
		t.epochIn, t.epochOut = p.epoch, t.lastEpoch+1
		t.boundary, t.switched = p.sequence, true
		t.trimOrigin, t.trimFloor = 0, 0
		t.retained = nil
	}
	seq := t.outputBase + (p.sequence - t.inputBase)
	epoch := t.epochOut + (p.epoch - t.epochIn)
	headerEpoch := epoch
	if t.switched && p.sequence == t.boundary {
		headerEpoch--
	}
	var out strings.Builder
	fmt.Fprintf(&out, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", seq, headerEpoch)
	inputSeq := p.sequence
	key := ""
	boundaryWritten := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "#EXTM3U" || strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") || strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:") {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-KEY:") {
			key = line
			if strings.Contains(key, "METHOD=AES-128") && !strings.Contains(key, "IV=") {
				continue
			}
		}
		if line == "#EXT-X-DISCONTINUITY" {
			epoch++
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			if t.switched && inputSeq == t.boundary && !boundaryWritten {
				out.WriteString("#EXT-X-DISCONTINUITY\n")
				boundaryWritten = true
			}
			// AES-128 without an explicit IV uses the upstream sequence number.
			// Renumbering must not change the decryption IV.
			if strings.Contains(key, "METHOD=AES-128") && !strings.Contains(key, "IV=") {
				fmt.Fprintf(&out, "%s,IV=0x%032x\n", key, inputSeq)
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
		if !strings.HasPrefix(line, "#") {
			inputSeq++
		}
	}
	if end := seq + p.count - 1; end > t.last {
		t.last = end
	}
	t.lastStart = p.sequence
	t.lastInputEpoch = p.epoch
	t.identities = identities
	if epoch > t.lastEpoch {
		t.lastEpoch = epoch
	}
	result := out.String()
	t.retained = retainTimelineMedia(t.retained, overlapMedia(result, mediaIdentity))
	t.lastOutputStart, t.lastPlaylist = seq, result
	return result
}
