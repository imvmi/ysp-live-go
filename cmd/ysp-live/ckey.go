package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ckPlatform = 4330403
	ckAppVer   = "V8.22.1035.3031"
)

var (
	ckTEA                  = mustHex("59b2f7cf725ef43c34fdd7c123411ed3")
	ckGTEA                 = mustHex("110DBEC10C23E7D2E56A1CAD6914EF1B")
	ckXOR                  = []byte{0x84, 0x2e, 0xed, 0x08, 0xf0, 0x66, 0xe6, 0xea, 0x48, 0xb4, 0xca, 0xa9, 0x91, 0xed, 0x6f, 0xf3}
	ckGXOR                 = []byte{0xb3, 0xc9, 0x53, 0xa0, 0x69, 0x13, 0xad, 0x4d}
	bkH264                 = base64.StdEncoding.EncodeToString([]byte("H(30:1080,60:1080|30:1080,60:1080)"))
	randomSource io.Reader = rand.Reader
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func teaBlock(block, key []byte) []byte {
	y := binary.BigEndian.Uint32(block[:4])
	z := binary.BigEndian.Uint32(block[4:8])
	k0 := binary.BigEndian.Uint32(key[0:4])
	k1 := binary.BigEndian.Uint32(key[4:8])
	k2 := binary.BigEndian.Uint32(key[8:12])
	k3 := binary.BigEndian.Uint32(key[12:16])
	var sum uint32
	for range 16 {
		sum += 0x9e3779b9
		y += ((z << 4) + k0) ^ (z + sum) ^ ((z >> 5) + k1)
		z += ((y << 4) + k2) ^ (y + sum) ^ ((y >> 5) + k3)
	}
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out[:4], y)
	binary.BigEndian.PutUint32(out[4:], z)
	return out
}

func ckChecksum(buf []byte) uint32 {
	var v uint32
	for _, b := range buf {
		v = (0x83*v + uint32(b)) & 0x7fffffff
	}
	return v
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(randomSource, b)
	return b, err
}

func teaPacket(data, key []byte) ([]byte, error) {
	pad := (8 - ((len(data) + 10) % 8)) % 8
	r, err := randomBytes(3 + pad)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, 0, len(data)+10+pad)
	plain = append(plain, (r[0]&0xf8)|byte(pad))
	plain = append(plain, r[1:1+pad]...)
	plain = append(plain, r[1+pad:]...)
	plain = append(plain, data...)
	plain = append(plain, make([]byte, 7)...)

	out := make([]byte, 0, len(plain))
	previousPlain := make([]byte, 8)
	previousCipher := make([]byte, 8)
	for off := 0; off < len(plain); off += 8 {
		mixed := make([]byte, 8)
		for i := range 8 {
			mixed[i] = plain[off+i] ^ previousCipher[i]
		}
		enc := teaBlock(mixed, key)
		cipher := make([]byte, 8)
		for i := range 8 {
			cipher[i] = enc[i] ^ previousPlain[i]
		}
		out = append(out, cipher...)
		previousPlain, previousCipher = mixed, cipher
	}
	return out, nil
}

func appendLP(dst []byte, value string) []byte {
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(value)))
	return append(dst, value...)
}

func lastFive(value string) string {
	if len(value) < 5 {
		return ""
	}
	return value[len(value)-5:]
}

func ckeyGuard(ts int64, guid string) ([]byte, error) {
	body := binary.BigEndian.AppendUint32(nil, uint32(ts))
	body = appendLP(body, lastFive(guid))
	body = appendLP(body, lastFive("null"))
	body = appendLP(body, lastFive("null"))
	body = appendLP(body, "-1")
	plain := binary.BigEndian.AppendUint16(nil, uint16(len(body)))
	plain = append(plain, body...)
	enc, err := teaPacket(plain, ckGTEA)
	if err != nil {
		return nil, err
	}
	enc = binary.BigEndian.AppendUint32(enc, ckChecksum(plain))
	for i := range enc {
		enc[i] ^= ckGXOR[i%len(ckGXOR)]
	}
	upper := strings.ToUpper(hex.EncodeToString(enc))
	return []byte(upper), nil
}

type ckeyTicket struct {
	CKey   string
	GUID   string
	TS     int64
	FlowID string
}

func makeCKey(channelID string, now time.Time) (ckeyTicket, error) {
	guidBytes, err := randomBytes(16)
	if err != nil {
		return ckeyTicket{}, err
	}
	guid := hex.EncodeToString(guidBytes)
	ts := now.Unix()
	guard, err := ckeyGuard(ts, guid)
	if err != nil {
		return ckeyTicket{}, err
	}
	uidBytes, err := randomBytes(4)
	if err != nil {
		return ckeyTicket{}, err
	}
	uid := strings.ToUpper(hex.EncodeToString(uidBytes))

	body := mustHex("0000004200000004000004d2")
	body = binary.BigEndian.AppendUint32(body, ckPlatform)
	body = binary.BigEndian.AppendUint32(body, 0)
	body = binary.BigEndian.AppendUint32(body, uint32(ts))
	body = appendLP(body, "dcgh")
	body = appendLP(body, "_zj1A5Gh6QYcxWjIUGos2w==")
	body = appendLP(body, ckAppVer)
	body = appendLP(body, channelID)
	body = appendLP(body, guid)
	body = binary.BigEndian.AppendUint32(body, 1)
	body = binary.BigEndian.AppendUint32(body, 1)
	body = appendLP(body, uid)
	body = appendLP(body, "nil")
	body = appendLP(body, "57eab0c4-2c58-44c6-8ae9-dd2757525dc5")
	body = appendLP(body, "nil")
	body = appendLP(body, "v0.1.000")
	body = appendLP(body, "com.cctv.yangshipin.app.iphone")
	body = appendLP(body, strconv.Itoa(ckPlatform))
	body = appendLP(body, "ex_json_bus")
	body = appendLP(body, "ex_json_vs")
	body = appendLP(body, string(guard))

	pkt := binary.BigEndian.AppendUint16(nil, uint16(len(body)))
	pkt = append(pkt, body...)
	if len(pkt) < 22 {
		return ckeyTicket{}, errors.New("cKey packet unexpectedly short")
	}
	binary.BigEndian.PutUint32(pkt[18:22], ckChecksum(pkt))
	enc, err := teaPacket(pkt, ckTEA)
	if err != nil {
		return ckeyTicket{}, err
	}
	enc = binary.BigEndian.AppendUint32(enc, ckChecksum(pkt))
	for i := range enc {
		enc[i] ^= ckXOR[i%len(ckXOR)]
	}
	flowBytes, err := randomBytes(16)
	if err != nil {
		return ckeyTicket{}, err
	}
	flowBytes[6] = (flowBytes[6] & 0x0f) | 0x40
	flowBytes[8] = (flowBytes[8] & 0x3f) | 0x80
	flow := strings.ToUpper(hex.EncodeToString(flowBytes)) + "_" + strconv.Itoa(ckPlatform)
	// The original client uses a non-standard URL-safe alphabet:
	// '+' becomes '_' and '/' becomes '-'. This is intentionally the reverse
	// of encoding/base64.RawURLEncoding's substitutions.
	encoded := base64.RawStdEncoding.EncodeToString(enc)
	encoded = strings.NewReplacer("+", "_", "/", "-").Replace(encoded)
	return ckeyTicket{
		CKey:   "--01" + encoded,
		GUID:   guid,
		TS:     ts,
		FlowID: flow,
	}, nil
}

func jsonCode(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	default:
		return -1
	}
}

func addURLValue(out *[]string, value any) {
	switch x := value.(type) {
	case string:
		for _, item := range strings.FieldsFunc(x, func(r rune) bool { return r == ';' || r == ',' }) {
			if s := strings.TrimSpace(item); s != "" {
				*out = append(*out, s)
			}
		}
	case []any:
		for _, item := range x {
			addURLValue(out, item)
		}
	case map[string]any:
		if u, ok := x["url"].(string); ok {
			addURLValue(out, u)
		} else if u, ok := x["playurl"].(string); ok {
			addURLValue(out, u)
		}
	}
}

func allowedCCTVURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "cctv.cn" || host == "cctv.com" || strings.HasSuffix(host, ".cctv.cn") || strings.HasSuffix(host, ".cctv.com")
}

func bkPlayURLs(ctx context.Context, client *http.Client, channelID, livePID, defn string, now time.Time) ([]string, error) {
	ticket, err := makeCKey(channelID, now)
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"atime":          {"120"},
		"livepid":        {livePID},
		"cnlid":          {channelID},
		"appVer":         {ckAppVer},
		"app_version":    {"300090"},
		"caplv":          {"1"},
		"cmd":            {"2"},
		"defn":           {defn},
		"device":         {"iPhone"},
		"encryptVer":     {"4.2"},
		"getpreviewinfo": {"0"},
		"hevclv":         {"0"},
		"lang":           {"zh-Hans_CN"},
		"livequeue":      {"0"},
		"logintype":      {"1"},
		"nettype":        {"1"},
		"newnettype":     {"1"},
		"newplatform":    {strconv.Itoa(ckPlatform)},
		"platform":       {strconv.Itoa(ckPlatform)},
		"sdtfrom":        {"v3021"},
		"spacode":        {"23"},
		"spaudio":        {"1"},
		"spdemuxer":      {"6"},
		"spdrm":          {"2"},
		"spdynamicrange": {"1"},
		"spflv":          {"1"},
		"spflvaudio":     {"1"},
		"sphdrfps":       {"60"},
		"sphttps":        {"1"},
		"spvcode":        {bkH264},
		"spvideo":        {"4"},
		"stream":         {"1"},
		"system":         {"1"},
		"sysver":         {"ios18.2.1"},
		"uhd_flag":       {"0"},
		"cKey":           {ticket.CKey},
		"guid":           {ticket.GUID},
		"fntick":         {strconv.FormatInt(ticket.TS, 10)},
		"flowid":         {ticket.FlowID},
		"playbacktime":   {"0"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://bkliveinfo.ysp.cctv.cn/?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "qqlive")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("bkliveinfo returned HTTP %d", resp.StatusCode)
	}
	var payload map[string]any
	body, err := readLimited(resp.Body, 8<<20)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return nil, err
	}
	if code := jsonCode(payload["iretcode"]); code != 0 {
		return nil, fmt.Errorf("iretcode=%d %v", code, payload["errinfo"])
	}
	var candidates []string
	addURLValue(&candidates, payload["playurl"])
	for _, key := range []string{"backurl_list", "backurlList", "backurl"} {
		addURLValue(&candidates, payload[key])
	}
	seen := make(map[string]bool)
	urls := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if !seen[candidate] && allowedCCTVURL(candidate) {
			seen[candidate] = true
			urls = append(urls, candidate)
		}
	}
	if len(urls) == 0 {
		return nil, errors.New("bkliveinfo returned no allowed play URL")
	}
	sort.SliceStable(urls, func(i, j int) bool {
		ib := strings.Contains(urls[i], "bklive-")
		jb := strings.Contains(urls[j], "bklive-")
		if ib != jb {
			return ib
		}
		// Retain the upstream's priority within a CDN class. The channel
		// worker learns which candidate actually performs best.
		return false
	})
	return urls, nil
}

func rewriteURIAttributes(line string, base *url.URL) string {
	const marker = `URI="`
	for start := 0; ; {
		i := strings.Index(line[start:], marker)
		if i < 0 {
			return line
		}
		i += start + len(marker)
		j := strings.IndexByte(line[i:], '"')
		if j < 0 {
			return line
		}
		j += i
		rel, err := url.Parse(line[i:j])
		if err == nil {
			absolute := base.ResolveReference(rel).String()
			line = line[:i] + absolute + line[j:]
			start = i + len(absolute) + 1
		} else {
			start = j + 1
		}
	}
}

func fetchAbsolutePlaylist(ctx context.Context, client *http.Client, rawURL string, depth int, bandwidthLimit int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "qqlive")
	req.Header.Set("Referer", "https://live.cctv.cn/")
	req.Header.Set("Accept", "application/vnd.apple.mpegurl,application/json,*/*")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("playlist returned HTTP %d", resp.StatusCode)
	}
	b, err := readLimited(resp.Body, 16<<20)
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	base := resp.Request.URL
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return "", errors.New("response is not an M3U8 playlist")
	}
	if strings.Contains(text, "#EXT-X-STREAM-INF:") {
		if depth >= 2 {
			return "", errors.New("master playlist nesting limit exceeded")
		}
		variant, err := selectVariant(lines, bandwidthLimit)
		if err != nil {
			return "", err
		}
		rel, err := url.Parse(variant)
		if err != nil {
			return "", err
		}
		return fetchAbsolutePlaylist(ctx, client, base.ResolveReference(rel).String(), depth+1, bandwidthLimit)
	}
	if err := validateMediaPlaylist(text); err != nil {
		return "", err
	}
	for i, line := range lines {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "#") {
			lines[i] = rewriteURIAttributes(line, base)
			continue
		}
		rel, err := url.Parse(s)
		if err != nil {
			return "", err
		}
		lines[i] = base.ResolveReference(rel).String()
	}
	return strings.Join(lines, "\n"), nil
}

// A 200 response with a header alone, a nested master, or a truncated segment
// entry is not playable and must not refresh the cache's success timestamp.
func validateMediaPlaylist(text string) error {
	pending, found := false, false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			return errors.New("unresolved master playlist")
		}
		if value, ok := strings.CutPrefix(line, "#EXTINF:"); ok {
			duration, err := strconv.ParseFloat(strings.SplitN(value, ",", 2)[0], 64)
			if pending || err != nil || duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
				return errors.New("invalid segment duration")
			}
			pending = true
		} else if line != "" && !strings.HasPrefix(line, "#") {
			if !pending {
				return errors.New("media URI without duration")
			}
			pending, found = false, true
		}
	}
	if pending || !found {
		return errors.New("empty or incomplete media playlist")
	}
	return nil
}
