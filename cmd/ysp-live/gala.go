package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	gala8Key      = "4E2918885FD98109869D14E0231A0BF4"
	gala8IV       = "16B17E519DDD0CE5B79D7A63A4DD801C"
	gala8Endpoint = "https://playvv.yangshipin.cn/playvinfo"
	gala8AppVer   = "0.2.0"
	gala8Platform = "4330701"
	gala8UA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/100.0.4896.127 Safari/537.36 Edg/100.0.1185.44"
)

type galaSource struct {
	Slug       string
	Year       int
	Name       string
	VID        string
	Definition string
}

var galaSources = []galaSource{
	{Slug: "2020", Year: 2020, Name: "2020年春晚", VID: "h0000737kbs", Definition: "shd"},
	{Slug: "2022", Year: 2022, Name: "2022年春晚", VID: "c000097p48h", Definition: "shd"},
	{Slug: "2023", Year: 2023, Name: "2023年春晚", VID: "c000014paz2", Definition: "shd"},
	{Slug: "2024", Year: 2024, Name: "2024年春晚", VID: "b000087jq2f", Definition: "shd"},
	{Slug: "2026", Year: 2026, Name: "2026年春晚", VID: "z000010y9p2", Definition: "shd"},
}

func galaSourceBySlug(slug string) (galaSource, bool) {
	for _, source := range galaSources {
		if source.Slug == slug {
			return source, true
		}
	}
	return galaSource{}, false
}

type galaResolver struct {
	client   *http.Client
	endpoint string
	now      func() time.Time
}

func newGalaResolver(client *http.Client) *galaResolver {
	return &galaResolver{client: client, endpoint: gala8Endpoint, now: time.Now}
}

func galaHash32(s string) int32 {
	var h uint32
	for _, r := range s {
		h = (h << 5) - h + uint32(r)
	}
	return int32(h)
}

func randomBase36(n int) (string, error) {
	b, err := randomBytes(n)
	if err != nil {
		return "", err
	}
	var value uint64
	for _, v := range b {
		value = value<<8 | uint64(v)
	}
	return strconv.FormatUint(value, 36), nil
}

func galaGUID(now time.Time) (string, error) {
	random, err := randomBase36(8)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(now.UnixMilli(), 36) + "_" + random, nil
}

func galaCKey(vid, appVer, guid string, timestamp int64) (string, error) {
	plain := fmt.Sprintf("|%d|%s|%d|mg3c3b04ba|%s|%s|%s|https://w.yangshipin.cn/|mozilla/5.0 (windows nt ||Mozilla|Netscape|Win32|",
		galaHash32(fmt.Sprintf("|%s|%d|mg3c3b04ba|%s|%s|%s|https://w.yangshipin.cn/|mozilla/5.0 (windows nt ||Mozilla|Netscape|Win32|", vid, timestamp, appVer, guid, gala8Platform)),
		vid, timestamp, appVer, guid, gala8Platform)
	key, _ := hexDecode(gala8Key)
	iv, _ := hexDecode(gala8IV)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := append([]byte(plain), make([]byte, pad)...)
	for i := len(plain); i < len(data); i++ {
		data[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, data)
	return "--01" + strings.ToUpper(fmt.Sprintf("%X", data)), nil
}

func hexDecode(s string) ([]byte, error) {
	const digits = "0123456789ABCDEF"
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		hi := strings.IndexByte(digits, s[i])
		lo := strings.IndexByte(digits, s[i+1])
		if hi < 0 || lo < 0 {
			return nil, errors.New("invalid hex")
		}
		out[i/2] = byte(hi<<4 | lo)
	}
	return out, nil
}

type galaResponse struct {
	ErrorCode int    `json:"em"`
	Message   string `json:"errinfo"`
	VideoList struct {
		Videos []struct {
			Title string `json:"ti"`
			File  string `json:"fn"`
			VKey  string `json:"fvkey"`
			Bases struct {
				Items []struct {
					URL string `json:"url"`
				} `json:"ui"`
			} `json:"ul"`
		} `json:"vi"`
	} `json:"vl"`
}

func parseGalaJSONP(body []byte) (galaResponse, error) {
	text := strings.TrimSpace(string(body))
	start, end := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if start < 0 || end <= start {
		return galaResponse{}, errors.New("invalid playvinfo JSONP")
	}
	var payload galaResponse
	if err := json.Unmarshal([]byte(text[start+1:end]), &payload); err != nil {
		return galaResponse{}, err
	}
	if payload.ErrorCode != 0 {
		return galaResponse{}, fmt.Errorf("playvinfo error %d: %s", payload.ErrorCode, payload.Message)
	}
	return payload, nil
}

func validGalaMediaURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "ysp.cctv.cn" || strings.HasSuffix(host, ".ysp.cctv.cn")
}

func (g *galaResolver) resolve(ctx context.Context, source galaSource) (string, error) {
	now := g.now()
	guid, err := galaGUID(now)
	if err != nil {
		return "", err
	}
	timestamp := now.Unix()
	ckey, err := galaCKey(source.VID, gala8AppVer, guid, timestamp)
	if err != nil {
		return "", err
	}
	flowRandom, err := randomBase36(8)
	if err != nil {
		return "", err
	}
	flowID := strconv.FormatInt(now.UnixMilli(), 36) + "_" + flowRandom
	q := url.Values{
		"guid": {guid}, "platform": {gala8Platform}, "vid": {source.VID}, "defn": {source.Definition},
		"charge": {"0"}, "defaultfmt": {"auto"}, "otype": {"json"}, "defnpayver": {"1"},
		"appVer": {gala8AppVer}, "sphttps": {"1"}, "sphls": {"1"}, "spwm": {"4"},
		"dtype": {"1"}, "defsrc": {"1"}, "encryptVer": {"8.1"}, "sdtfrom": {gala8Platform},
		"cKey": {ckey}, "flowid": {flowID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", gala8UA)
	req.Header.Set("Referer", "https://w.yangshipin.cn/")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("playvinfo returned HTTP %d", resp.StatusCode)
	}
	body, err := readLimited(resp.Body, 4<<20)
	if err != nil {
		return "", err
	}
	payload, err := parseGalaJSONP(body)
	if err != nil {
		return "", err
	}
	if len(payload.VideoList.Videos) == 0 {
		return "", errors.New("playvinfo returned no video")
	}
	for _, video := range payload.VideoList.Videos {
		if video.File == "" || video.VKey == "" || len(video.Bases.Items) == 0 {
			continue
		}
		base, err := url.Parse(video.Bases.Items[0].URL)
		if err != nil || base.Scheme != "https" || !validGalaMediaURL(video.Bases.Items[0].URL) {
			continue
		}
		media := strings.TrimRight(base.String(), "/") + "/" + video.File
		mu, err := url.Parse(media)
		if err != nil {
			continue
		}
		params := url.Values{"vkey": {video.VKey}, "guid": {guid}, "sdtfrom": {gala8Platform}, "platform": {"2"}}
		mu.RawQuery = params.Encode()
		return mu.String(), nil
	}
	return "", errors.New("playvinfo returned no valid media URL")
}
