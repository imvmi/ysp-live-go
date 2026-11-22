package main

import (
	"fmt"
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
	p := mediaPosition(text)
	if p.count == 0 {
		return ""
	}
	identities := make(map[uint64]string, p.count)
	inputIndex := p.sequence
	byteRange := ""
	conflict := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-BYTERANGE:") {
			byteRange = line
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		identity := segmentKey(line, "") + "|" + byteRange
		identities[inputIndex] = identity
		if old, ok := t.identities[inputIndex]; ok && old != identity {
			conflict = true
		}
		inputIndex++
		byteRange = ""
	}
	if t.source == "" {
		t.source, t.inputBase, t.outputBase = source, p.sequence, p.sequence
		t.epochIn, t.epochOut = p.epoch, p.epoch
	} else if t.source != source || p.sequence < t.lastStart || p.epoch < t.lastInputEpoch || conflict {
		t.source, t.inputBase, t.outputBase = source, p.sequence, t.last+1
		t.epochIn, t.epochOut = p.epoch, t.lastEpoch+1
		t.boundary, t.switched = p.sequence, true
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
	t.last = seq + p.count - 1
	t.lastStart = p.sequence
	t.lastInputEpoch = p.epoch
	t.identities = identities
	t.lastEpoch = epoch
	return out.String()
}
