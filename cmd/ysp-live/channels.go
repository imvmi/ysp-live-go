package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"

type channelSpec struct {
	Slug string
	Name string
	SID  string
	PID  string
	Defn string
}

var channelSpecs = []channelSpec{
	{"cctv1", "CCTV-1", "2024078201", "600001859", "fhd"},
	{"cctv2", "CCTV-2", "2024075401", "600001800", "fhd"},
	{"cctv3", "CCTV-3", "2024068501", "600001801", "fhd"},
	{"cctv4", "CCTV-4", "2029797101", "600001814", "fhd"},
	{"cctv5", "CCTV-5", "2024078401", "600001818", "fhd"},
	{"cctv5p", "CCTV-5+", "2024078001", "600001817", "fhd"},
	{"cctv6", "CCTV-6", "2013693901", "600108442", "fhd"},
	{"cctv7", "CCTV-7", "2024072001", "600004092", "fhd"},
	{"cctv8", "CCTV-8", "2029793001", "600001803", "fhd"},
	{"cctv9", "CCTV-9", "2024078601", "600004078", "fhd"},
	{"cctv10", "CCTV-10", "2024078701", "600001805", "fhd"},
	{"cctv11", "CCTV-11", "2027248701", "600001806", "fhd"},
	{"cctv12", "CCTV-12", "2027248801", "600001807", "fhd"},
	{"cctv13", "CCTV-13", "2029797201", "600001811", "fhd"},
	{"cctv14", "CCTV-14", "2027248901", "600001809", "fhd"},
	{"cctv15", "CCTV-15", "2027249001", "600001815", "fhd"},
	{"cctv16", "CCTV-16", "2027249101", "600098637", "fhd"},
	{"cctv164k", "CCTV-16 4K", "2027249301", "600099502", "fhd"},
	{"cctv17", "CCTV-17", "2027249401", "600001810", "fhd"},
	{"cctv4k", "CCTV-4K", "2029810301", "600002264", "fhd"},
	{"cctv8k", "CCTV-8K", "2026774101", "600156816", "fhd"},
	{"cgtn", "CGTN", "2024181701", "600014550", "fhd"},
	{"cgtnfr", "CGTN法语", "2024181801", "600084704", "fhd"},
	{"cgtnru", "CGTN俄语", "2024181901", "600084758", "fhd"},
	{"cgtnar", "CGTN阿拉伯语", "2024182001", "600084782", "fhd"},
	{"cgtnes", "CGTN西班牙语", "2024182101", "600084744", "fhd"},
	{"cgtndoc", "CGTN 纪录", "2024182301", "600084781", "fhd"},
	{"cctvfyjc", "CCTV 风云剧场", "2025637103", "600099658", "shd"},
	{"cctvdyjc", "CCTV 第一剧场", "2026874203", "600099655", "shd"},
	{"cctvhjjc", "CCTV 怀旧剧场", "2026874303", "600099620", "shd"},
	{"bjws", "北京卫视", "2024052703", "600002309", "fhd"},
	{"jsws", "江苏卫视", "2024171103", "600002521", "fhd"},
	{"dfws", "东方卫视", "2024054503", "600002483", "fhd"},
	{"zjws", "浙江卫视", "2024054703", "600002520", "fhd"},
	{"hnws", "湖南卫视", "2024054803", "600002475", "fhd"},
	{"hbws", "湖北卫视", "2024171203", "600002508", "fhd"},
	{"gdws", "广东卫视", "2024060903", "600002485", "fhd"},
	{"gxws", "广西卫视", "2024060703", "600002509", "fhd"},
	{"hljws", "黑龙江卫视", "2029797003", "600002498", "fhd"},
	{"hainanws", "海南卫视", "2024055603", "600002506", "fhd"},
	{"cqws", "重庆卫视", "2024061103", "600002531", "fhd"},
	{"szws", "深圳卫视", "2024061303", "600002481", "fhd"},
	{"scws", "四川卫视", "2024061403", "600002516", "fhd"},
	{"henanws", "河南卫视", "2029797303", "600002525", "fhd"},
	{"dnws", "东南卫视", "2024061503", "600002484", "fhd"},
	{"gzws", "贵州卫视", "2024061603", "600002490", "fhd"},
	{"jxws", "江西卫视", "2024061703", "600002503", "fhd"},
	{"lnws", "辽宁卫视", "2024171303", "600002505", "fhd"},
	{"ahws", "安徽卫视", "2024171403", "600002532", "fhd"},
	{"hebws", "河北卫视", "2024171503", "600002493", "fhd"},
	{"sdws", "山东卫视", "2029787903", "600002513", "fhd"},
	{"tjws", "天津卫视", "2019927003", "600152137", "fhd"},
	{"jlws", "吉林卫视", "2025561503", "600190405", "fhd"},
	{"saxws", "陕西卫视", "2029795103", "600190400", "fhd"},
	{"nxws", "宁夏卫视", "2025608503", "600190737", "fhd"},
	{"nmgws", "内蒙古卫视", "2025561203", "600190401", "fhd"},
	{"ynws", "云南卫视", "2025561303", "600190402", "fhd"},
	{"shanxiws", "山西卫视", "2025560803", "600190407", "fhd"},
	{"qhws", "青海卫视", "2025559103", "600190406", "fhd"},
	{"xizangws", "西藏卫视", "2025558003", "600190403", "fhd"},
	{"xjws", "新疆卫视", "2019927403", "600152138", "fhd"},
	{"cetv1", "CETV-1", "2022823801", "600171827", "fhd"},
	{"guoxue", "国学频道", "2029360403", "600213139", "fhd"},
}

var (
	forceBK         = stringSet("cctv11", "cctv12", "cctv14", "cctv15", "cctv16", "cctv164k", "cctv17", "cctv4k", "cctvfyjc", "cctvdyjc", "cctvhjjc")
	backendChannels = stringSet(
		"cctv1", "cctv2", "cctv3", "cctv4", "cctv5", "cctv5p",
		"cctv7", "cctv8", "cctv9", "cctv10", "cctv11", "cctv12", "cctv13", "cctv14", "cctv15", "cctv16", "cctv17",
		"cctv4k", "cctv8k", "cctv164k", "cgtn", "cgtnfr", "cgtnru", "cgtnar", "cgtnes", "cgtndoc",
	)
	true4KChannels = stringSet("cctv4k", "cctv8k", "cctv164k")
)

func stringSet(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, value := range values {
		m[value] = true
	}
	return m
}

type segment struct {
	Sequence uint64
	Duration float64
	PDT      string
	URL      string
}

type channel struct {
	spec channelSpec

	mu            sync.Mutex
	segments      map[string]*segment
	order         []string
	sequence      uint64
	lastAccess    time.Time
	workerRunning bool
	starting      bool
	lastError     string
	lastOK        time.Time
	mode          string
	bkURLs        []string
	bkURLsTime    time.Time
	bkPlaylist    string
}

type channelManager struct {
	client          *http.Client
	guid            string
	channels        map[string]*channel
	refreshInterval time.Duration
	idleTimeout     time.Duration
	window          time.Duration
	maxSegments     int
	bkURLTTL        time.Duration
}

func newChannelManager(client *http.Client) *channelManager {
	m := &channelManager{
		client:          client,
		guid:            newGUID(),
		channels:        make(map[string]*channel, len(channelSpecs)),
		refreshInterval: 10 * time.Second,
		idleTimeout:     120 * time.Second,
		window:          300 * time.Second,
		maxSegments:     60,
		bkURLTTL:        300 * time.Second,
	}
	for _, spec := range channelSpecs {
		mode := "jce"
		if forceBK[spec.Slug] {
			mode = "bk"
		}
		m.channels[spec.Slug] = &channel{
			spec:     spec,
			segments: make(map[string]*segment),
			mode:     mode,
		}
	}
	return m
}

func segmentKey(rawURL, pdt string) string {
	if pdt != "" {
		return "pdt:" + pdt
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func (m *channelManager) jceFetch(ctx context.Context, ch *channel) ([]segment, error) {
	now := time.Now()
	playlistURL, err := jceTimeshiftURL(ctx, m.client, m.guid, ch.spec.PID, ch.spec.SID, now.Add(-m.window).Unix(), now.Unix(), ch.spec.Defn)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playlistURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("JCE playlist returned HTTP %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 16<<20))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	base := resp.Request.URL
	var result []segment
	duration := 6.0
	pdt := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			value := strings.TrimPrefix(line, "#EXTINF:")
			value = strings.SplitN(value, ",", 2)[0]
			if n, err := strconv.ParseFloat(value, 64); err == nil {
				duration = n
			}
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			pdt = strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:")
		case line != "" && !strings.HasPrefix(line, "#"):
			rel, err := url.Parse(line)
			if err != nil {
				return nil, err
			}
			result = append(result, segment{Duration: duration, PDT: pdt, URL: base.ResolveReference(rel).String()})
			pdt = ""
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, errors.New("empty JCE playlist")
	}
	return result, nil
}

func (m *channelManager) jceRefresh(ctx context.Context, ch *channel) error {
	segments, err := m.jceFetch(ctx, ch)
	if err != nil {
		return err
	}
	ch.mu.Lock()
	added := 0
	for _, item := range segments {
		key := segmentKey(item.URL, item.PDT)
		if current := ch.segments[key]; current != nil {
			current.URL = item.URL
			continue
		}
		ch.sequence++
		item.Sequence = ch.sequence
		copyItem := item
		ch.segments[key] = &copyItem
		ch.order = append(ch.order, key)
		added++
	}
	for len(ch.order) > m.maxSegments {
		delete(ch.segments, ch.order[0])
		ch.order = ch.order[1:]
	}
	total := len(ch.segments)
	ch.lastError = ""
	ch.mu.Unlock()
	if added > 0 {
		log.Printf("%s +%d segments (%d total)", ch.spec.Slug, added, total)
	}
	return nil
}

func (m *channelManager) bkRefresh(ctx context.Context, ch *channel) error {
	ch.mu.Lock()
	needURLs := len(ch.bkURLs) == 0 || time.Since(ch.bkURLsTime) > m.bkURLTTL
	urls := append([]string(nil), ch.bkURLs...)
	ch.mu.Unlock()
	if needURLs {
		fresh, err := bkPlayURLs(ctx, m.client, ch.spec.SID, ch.spec.PID, ch.spec.Defn, time.Now())
		if err != nil {
			return err
		}
		ch.mu.Lock()
		ch.bkURLs = append([]string(nil), fresh...)
		ch.bkURLsTime = time.Now()
		urls = append([]string(nil), fresh...)
		ch.mu.Unlock()
		log.Printf("%s bkliveinfo returned %d URLs", ch.spec.Slug, len(urls))
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		for _, candidate := range urls {
			playlist, err := fetchAbsolutePlaylist(ctx, m.client, candidate, 0)
			if err != nil {
				lastErr = err
				continue
			}
			if !strings.Contains(playlist, "#EXTM3U") {
				lastErr = errors.New("response is not an M3U8 playlist")
				continue
			}
			ch.mu.Lock()
			ch.bkPlaylist = playlist
			ch.lastError = ""
			ch.mu.Unlock()
			return nil
		}
		if attempt == 0 {
			fresh, err := bkPlayURLs(ctx, m.client, ch.spec.SID, ch.spec.PID, ch.spec.Defn, time.Now())
			if err == nil {
				urls = fresh
				ch.mu.Lock()
				ch.bkURLs = append([]string(nil), fresh...)
				ch.bkURLsTime = time.Now()
				ch.mu.Unlock()
				log.Printf("%s refreshed expired backup URLs", ch.spec.Slug)
			} else {
				lastErr = err
			}
		}
	}
	ch.mu.Lock()
	ch.bkURLsTime = time.Time{}
	ch.mu.Unlock()
	if lastErr == nil {
		lastErr = errors.New("backup playlist failed")
	}
	return lastErr
}

func (m *channelManager) refreshOnce(ctx context.Context, ch *channel) bool {
	ch.mu.Lock()
	mode := ch.mode
	ch.mu.Unlock()
	var err error
	if mode == "bk" {
		err = m.bkRefresh(ctx, ch)
	} else {
		err = m.jceRefresh(ctx, ch)
		var dead *deadHostError
		if errors.As(err, &dead) {
			ch.mu.Lock()
			ch.mode = "bk"
			ch.mu.Unlock()
			log.Printf("%s JCE returned dead host; switching to bkliveinfo", ch.spec.Slug)
			err = m.bkRefresh(ctx, ch)
		}
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if err != nil {
		ch.lastError = truncateError(err, 120)
		log.Printf("%s refresh failed: %s", ch.spec.Slug, ch.lastError)
		return false
	}
	ch.lastOK = time.Now()
	ch.lastError = ""
	return true
}

func truncateError(err error, limit int) string {
	s := fmt.Sprintf("%T: %v", err, err)
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

func (m *channelManager) refreshLoop(ch *channel) {
	defer func() {
		ch.mu.Lock()
		ch.workerRunning = false
		ch.mu.Unlock()
	}()
	log.Printf("%s background refresh started", ch.spec.Slug)
	failures := 0
	for {
		ch.mu.Lock()
		lastAccess := ch.lastAccess
		ch.mu.Unlock()
		if time.Since(lastAccess) >= m.idleTimeout {
			log.Printf("%s idle; stopping refresh", ch.spec.Slug)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		ok := m.refreshOnce(ctx, ch)
		cancel()
		if ok {
			failures = 0
		} else {
			failures++
		}
		delay := m.refreshInterval
		if failures >= 3 {
			delay = 30 * time.Second
		}
		time.Sleep(delay)
	}
}

func (m *channelManager) ensure(ch *channel) {
	now := time.Now()
	ch.mu.Lock()
	ch.lastAccess = now
	if ch.starting {
		ch.mu.Unlock()
		return
	}
	needFetch := len(ch.segments) == 0 && ch.bkPlaylist == ""
	needWorker := !ch.workerRunning
	if !needFetch && !needWorker {
		ch.mu.Unlock()
		return
	}
	ch.starting = true
	ch.mu.Unlock()

	if needFetch {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		m.refreshOnce(ctx, ch)
		cancel()
	}
	ch.mu.Lock()
	if needWorker && !ch.workerRunning {
		ch.workerRunning = true
		go m.refreshLoop(ch)
	}
	ch.starting = false
	ch.mu.Unlock()
}

func (m *channelManager) buildPlaylist(ch *channel) string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.mode == "bk" {
		return ch.bkPlaylist
	}
	start := 0
	if len(ch.order) > 15 {
		start = len(ch.order) - 15
	}
	items := make([]*segment, 0, len(ch.order)-start)
	for _, key := range ch.order[start:] {
		if item := ch.segments[key]; item != nil {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return ""
	}
	target := 6
	for _, item := range items {
		if n := int(item.Duration + 0.5); n > target {
			target = n
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", target, items[0].Sequence)
	for _, item := range items {
		if item.PDT != "" {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", item.PDT)
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", item.Duration, item.URL)
	}
	return b.String()
}

type channelDiagnostic struct {
	Slug      string
	Mode      string
	LastOK    time.Time
	LastError string
}

func (m *channelManager) diagnostics() []channelDiagnostic {
	result := make([]channelDiagnostic, 0, len(channelSpecs))
	for _, spec := range channelSpecs {
		ch := m.channels[spec.Slug]
		ch.mu.Lock()
		result = append(result, channelDiagnostic{Slug: spec.Slug, Mode: ch.mode, LastOK: ch.lastOK, LastError: ch.lastError})
		ch.mu.Unlock()
	}
	return result
}
