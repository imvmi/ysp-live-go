package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
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
	Sequence      uint64
	Epoch         uint64
	SourceEpoch   uint64
	Discontinuity bool
	Duration      float64
	PDT           string
	URL           string
}

type channel struct {
	spec channelSpec

	mu            sync.Mutex
	segments      map[string]*segment
	order         []string
	sequence      uint64
	lastAccess    time.Time
	workerRunning bool
	refreshing    bool
	updated       chan struct{}
	wake          chan struct{}
	nextRetry     time.Time
	lastError     string
	lastOK        time.Time
	lastProgress  time.Time
	lastMedia     string
	mode          string
	bkURLs        []string
	bkSources     map[string]sourceState
	bkPreferred   string
	bkPlaylist    string
}

type channelManager struct {
	mu              sync.Mutex // Serializes worker registration with Close.
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	stopped         bool
	maxPlaylistAge  time.Duration
	client          *http.Client
	guid            string
	channels        map[string]*channel
	refreshInterval time.Duration
	idleTimeout     time.Duration
	window          time.Duration
	maxSegments     int
	maxBandwidth    int64
	hedgeDelay      time.Duration
	sourceTimeout   time.Duration
	sourceCooldown  time.Duration
}

func newChannelManager(client *http.Client) *channelManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &channelManager{
		ctx: ctx, cancel: cancel, maxPlaylistAge: 30 * time.Second,
		client:          client,
		guid:            newGUID(),
		channels:        make(map[string]*channel, len(channelSpecs)),
		refreshInterval: 10 * time.Second,
		idleTimeout:     120 * time.Second,
		window:          300 * time.Second,
		maxSegments:     60,
		maxBandwidth:    defaultMaxBandwidth,
		hedgeDelay:      250 * time.Millisecond,
		sourceTimeout:   5 * time.Second,
		sourceCooldown:  10 * time.Second,
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
			updated:  make(chan struct{}),
			wake:     make(chan struct{}, 1),
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
	body, err := readLimited(resp.Body, 16<<20)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(body), "#EXTM3U\n") && !strings.HasPrefix(string(body), "#EXTM3U\r\n") {
		return nil, errors.New("response is not an M3U8 playlist")
	}
	if err := validateMediaPlaylist(string(body)); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	base := resp.Request.URL
	var result []segment
	duration := 6.0
	pdt := ""
	sourceEpoch := uint64(0)
	discontinuity := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			n, err := strconv.ParseUint(strings.TrimPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"), 10, 64)
			if err != nil || len(result) > 0 || discontinuity {
				return nil, errors.New("invalid discontinuity sequence")
			}
			sourceEpoch = n
		case line == "#EXT-X-DISCONTINUITY":
			if sourceEpoch == math.MaxUint64 {
				return nil, errors.New("discontinuity sequence overflow")
			}
			sourceEpoch++
			discontinuity = true
		case strings.HasPrefix(line, "#EXTINF:"):
			value := strings.TrimPrefix(line, "#EXTINF:")
			value = strings.SplitN(value, ",", 2)[0]
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, errors.New("invalid JCE segment duration")
			}
			duration = n
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			pdt = strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:")
		case line != "" && !strings.HasPrefix(line, "#"):
			rel, err := url.Parse(line)
			if err != nil {
				return nil, err
			}
			result = append(result, segment{Duration: duration, PDT: pdt, URL: base.ResolveReference(rel).String(), SourceEpoch: sourceEpoch, Discontinuity: discontinuity})
			pdt = ""
			discontinuity = false
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
	return m.mergeSegments(ch, segments)
}

// Reconcile the upstream window, rather than re-adding evicted segments on
// every poll when a five-minute window contains more than maxSegments items.
func (m *channelManager) mergeSegments(ch *channel, segments []segment) error {
	if len(segments) > m.maxSegments {
		segments = segments[len(segments)-m.maxSegments:]
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	lastKnown := -1
	for i, item := range segments {
		if ch.segments[segmentKey(item.URL, item.PDT)] != nil {
			lastKnown = i
		}
	}
	if len(segments) == 0 {
		return errors.New("empty segment window")
	}
	// Anchor local epochs to a retained segment. Upstream discontinuities must
	// survive refreshes and remain stable when their marker leaves the window.
	sourceAnchor, localAnchor := segments[0].SourceEpoch, segments[0].SourceEpoch
	if lastKnown >= 0 {
		anchor := segments[lastKnown]
		sourceAnchor = anchor.SourceEpoch
		localAnchor = ch.segments[segmentKey(anchor.URL, anchor.PDT)].Epoch
	} else if len(ch.order) > 0 {
		previous := ch.segments[ch.order[len(ch.order)-1]].Epoch
		if previous == math.MaxUint64 {
			return errors.New("discontinuity sequence overflow")
		}
		localAnchor = max(localAnchor, previous+1)
	}
	if len(segments) > 0 && len(ch.order) > 0 {
		last := segments[len(segments)-1]
		if old := ch.segments[segmentKey(last.URL, last.PDT)]; old != nil && old.Sequence < ch.sequence {
			return errors.New("JCE playlist moved backwards")
		}
	}
	nextSequence := ch.sequence
	next := make(map[string]*segment, len(segments))
	order := make([]string, 0, len(segments))
	for i, item := range segments {
		key := segmentKey(item.URL, item.PDT)
		if next[key] != nil {
			continue
		}
		if item.SourceEpoch >= sourceAnchor {
			delta := item.SourceEpoch - sourceAnchor
			if delta > math.MaxUint64-localAnchor {
				return errors.New("discontinuity sequence overflow")
			}
			item.Epoch = localAnchor + delta
		} else {
			delta := sourceAnchor - item.SourceEpoch
			if delta > localAnchor {
				return errors.New("discontinuity sequence moved backwards")
			}
			item.Epoch = localAnchor - delta
		}
		if current := ch.segments[key]; current != nil {
			if item.Epoch != current.Epoch {
				return errors.New("upstream changed a retained segment's timeline")
			}
			item.Sequence = current.Sequence
		} else {
			if i < lastKnown {
				continue // An older segment must not acquire a new sequence number.
			}
			nextSequence++
			item.Sequence = nextSequence
		}
		if len(order) > 0 && (item.Epoch < next[order[len(order)-1]].Epoch || item.Epoch-next[order[len(order)-1]].Epoch > 1024) {
			return errors.New("invalid discontinuity jump")
		}
		if len(order) > 0 && item.Sequence != next[order[len(order)-1]].Sequence+1 {
			return errors.New("JCE playlist contains a sequence gap")
		}
		copyItem := item
		next[key] = &copyItem
		order = append(order, key)
	}
	ch.segments, ch.order, ch.sequence = next, order, nextSequence
	return nil
}

func (m *channelManager) discoverBKURLs(ctx context.Context, ch *channel) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return bkPlayURLs(ctx, m.client, ch.spec.SID, ch.spec.PID, ch.spec.Defn, time.Now())
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
	if err == nil {
		media := ""
		if ch.mode == "bk" {
			media = playlistMediaKey(ch.bkPlaylist)
		} else if len(ch.order) > 0 {
			media = ch.order[len(ch.order)-1]
		}
		if media != ch.lastMedia || ch.lastProgress.IsZero() {
			ch.lastMedia, ch.lastProgress = media, time.Now()
		} else if time.Since(ch.lastProgress) >= m.maxPlaylistAge {
			err = errors.New("upstream playlist stopped advancing")
		}
	}
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

// Close cancels both in-flight upstream requests and sleeping refresh workers.
func (m *channelManager) Close() {
	m.mu.Lock()
	m.stopped = true
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}

// Caller holds ch.mu. A nonempty cache alone does not imply playable media.
func (m *channelManager) freshLocked(ch *channel) bool {
	hasData := len(ch.order) > 0
	if ch.mode == "bk" {
		hasData = ch.bkPlaylist != ""
	}
	ageLimit := m.maxPlaylistAge
	if ch.mode == "bk" {
		if target := playlistTargetDuration(ch.bkPlaylist); target > 0 && 2*target < ageLimit {
			ageLimit = 2 * target
		}
	}
	return hasData && !ch.lastOK.IsZero() && time.Since(ch.lastOK) < ageLimit &&
		!ch.lastProgress.IsZero() && time.Since(ch.lastProgress) < m.maxPlaylistAge
}

func (m *channelManager) refreshLoop(ch *channel) {
	defer m.wg.Done()
	log.Printf("%s background refresh started", ch.spec.Slug)
	failures := 0
	for {
		ch.mu.Lock()
		if m.ctx.Err() != nil || time.Since(ch.lastAccess) >= m.idleTimeout {
			// The access check and ownership release must be atomic with ensure.
			ch.workerRunning = false
			ch.mu.Unlock()
			return
		}
		ch.refreshing = true
		// A wake queued before this refresh is already satisfied by this attempt.
		select {
		case <-ch.wake:
		default:
		}
		ch.mu.Unlock()
		ctx, cancel := context.WithTimeout(m.ctx, 35*time.Second)
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
		ch.mu.Lock()
		if ok && ch.mode == "bk" {
			// Short live windows need polling at the upstream segment cadence.
			if target := playlistTargetDuration(ch.bkPlaylist); target > 0 && target < delay {
				delay = target
			}
		}
		ch.refreshing = false
		ch.nextRetry = time.Now().Add(delay)
		close(ch.updated)
		ch.updated = make(chan struct{})
		ch.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-m.ctx.Done():
		case <-timer.C:
		case <-ch.wake:
		}
		timer.Stop()
	}
}

func playlistMediaKey(playlist string) string {
	sequence, lastURI := "", ""
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			sequence = line
		}
		if line != "" && !strings.HasPrefix(line, "#") {
			lastURI = segmentKey(line, "")
		}
	}
	return sequence + "|" + lastURI
}

func playlistTargetDuration(playlist string) time.Duration {
	for _, line := range strings.Split(playlist, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "#EXT-X-TARGETDURATION:"); ok {
			if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 && seconds <= 3600 {
				return time.Duration(seconds) * time.Second
			}
		}
	}
	return 0
}

func (m *channelManager) ensure(ctx context.Context, ch *channel) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return context.Canceled
	}
	ch.mu.Lock()
	ch.lastAccess = time.Now()
	started := !ch.workerRunning
	if started {
		ch.workerRunning = true
		ch.nextRetry = time.Time{}
		m.wg.Add(1)
		go m.refreshLoop(ch)
	}
	m.mu.Unlock()
	if m.freshLocked(ch) {
		ch.mu.Unlock()
		return nil
	}
	if !started && !ch.refreshing && time.Now().Before(ch.nextRetry) && ch.lastError != "" {
		err := errors.New(ch.lastError)
		ch.mu.Unlock()
		return err
	}
	updated := ch.updated
	if !started && !ch.refreshing {
		select {
		case ch.wake <- struct{}{}:
		default:
		}
	}
	ch.mu.Unlock()
	// All callers wait for the same worker; cancelling one client must not
	// cancel a shared refresh needed by other viewers.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return m.ctx.Err()
	case <-updated:
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if m.freshLocked(ch) {
		return nil
	}
	if ch.lastError != "" {
		return errors.New(ch.lastError)
	}
	return errors.New("playlist is stale")
}

func (m *channelManager) buildPlaylist(ch *channel) string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if !m.freshLocked(ch) {
		return ""
	}
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
	epoch := items[0].Epoch
	if items[0].Discontinuity && epoch > 0 {
		epoch--
	}
	fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", epoch)
	for _, item := range items {
		for epoch < item.Epoch {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
			epoch++
		}
		if item.PDT != "" {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", item.PDT)
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", item.Duration, item.URL)
	}
	return b.String()
}

type channelDiagnostic struct {
	Slug          string
	Mode          string
	LastOK        time.Time
	LastError     string
	WorkerRunning bool
	Fresh         bool
	LastProgress  time.Time
}

func (m *channelManager) diagnostics() []channelDiagnostic {
	result := make([]channelDiagnostic, 0, len(channelSpecs))
	for _, spec := range channelSpecs {
		ch := m.channels[spec.Slug]
		ch.mu.Lock()
		result = append(result, channelDiagnostic{Slug: spec.Slug, Mode: ch.mode, LastOK: ch.lastOK, LastError: ch.lastError, WorkerRunning: ch.workerRunning, Fresh: m.freshLocked(ch), LastProgress: ch.lastProgress})
		ch.mu.Unlock()
	}
	return result
}
