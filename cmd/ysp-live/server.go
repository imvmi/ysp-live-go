package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type appServer struct {
	channels *channelManager
	device   deviceSource
	gala     *galaResolver
	port     int
	bind     string
	index    *template.Template

	deviceMu       sync.Mutex
	deviceFailures int
	deviceRetryAt  time.Time
}

type indexChannel struct {
	Slug string
	Name string
	Tag  string
}

type indexData struct {
	Count        int
	BackendReady bool
	Channels     []indexChannel
	VODs         []indexVOD
}

type indexVOD struct {
	Name string
	Path string
}

const indexTemplate = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>央视频全频道直播</title>
<style>body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;max-width:760px;margin:0 auto;padding:20px}li{margin:6px 0}.tag{color:#b00020;font-weight:600}.path{color:#777;font-size:12px;margin-left:8px}</style>
</head><body>
<h2>央视频全频道直播（{{.Count}} 路{{if .BackendReady}}，含设备协议源{{end}}）</h2>
<p>将链接粘贴到播放器即可观看。普通频道的分片由播放器直连央视 CDN；设备协议频道可能经本地后端中继。</p>
<p>聚合订阅：<a href="/all.m3u">/all.m3u</a></p>
<h3>历届春晚点播</h3><ul>{{range .VODs}}<li><a href="{{.Path}}">{{.Name}}</a></li>{{end}}</ul>
<ul>{{range .Channels}}<li><a href="/{{.Slug}}.m3u8">{{.Name}}</a>{{if .Tag}} <span class="tag">[{{.Tag}}]</span>{{end}} <span class="path">/{{.Slug}}.m3u8</span></li>{{end}}</ul>
</body></html>`

func newAppServer(channels *channelManager, bind string, port int) *appServer {
	return &appServer{
		channels: channels,
		bind:     bind,
		port:     port,
		index:    template.Must(template.New("index").Parse(indexTemplate)),
		gala:     newGalaResolver(channels.client),
	}
}

func deviceRetryDelay(failures int) time.Duration {
	switch failures {
	case 1:
		return 15 * time.Second
	case 2:
		return time.Minute
	default:
		return 2 * time.Minute
	}
}

func (s *appServer) deviceAttemptAllowed() bool {
	if s.device == nil {
		return false
	}
	if s.device.Ready() {
		return true
	}
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	return !time.Now().Before(s.deviceRetryAt)
}

// Device session bootstrap is process-wide. A failure must cool down all
// channels, otherwise channel surfing repeats the same slow registration on
// every slug before falling back to BK.
func (s *appServer) recordDeviceAttempt(err error) {
	ready := s.device != nil && s.device.Ready()
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	if err == nil || ready {
		s.deviceFailures = 0
		s.deviceRetryAt = time.Time{}
		return
	}
	if errors.Is(err, errDeviceLinkQuota) || errors.Is(err, errDeviceCoolingDown) || errors.Is(err, context.Canceled) {
		return
	}
	s.deviceFailures++
	s.deviceRetryAt = time.Now().Add(deviceRetryDelay(s.deviceFailures))
}

func (s *appServer) write(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *appServer) publicHost(r *http.Request) string {
	host := advertisedHost(r.Host)
	if host == "" {
		host = s.bind
		if host == "0.0.0.0" || host == "::" || host == "" {
			host = "127.0.0.1"
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(s.port))
}

func advertisedHost(hostHeader string) string {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	} else if strings.Count(hostHeader, ":") == 1 {
		host = strings.SplitN(hostHeader, ":", 2)[0]
	}
	host = strings.Trim(host, "[] ")
	if host == "" {
		return ""
	}
	if net.ParseIP(host) != nil {
		return host
	}
	for _, r := range host {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '-' {
			return ""
		}
	}
	return host
}

func (s *appServer) serveIndex(w http.ResponseWriter) {
	ready := s.device != nil
	data := indexData{Count: len(channelSpecs), BackendReady: ready}
	for _, source := range galaSources {
		data.VODs = append(data.VODs, indexVOD{Name: source.Name, Path: "/vod/spring-gala/" + source.Slug + ".mp4"})
	}
	for _, spec := range channelSpecs {
		tag := ""
		if ready && true4KChannels[spec.Slug] {
			tag = "真 4K 备用"
		} else if ready && deviceChannels[spec.Slug] {
			tag = "高码率备用"
		}
		data.Channels = append(data.Channels, indexChannel{Slug: spec.Slug, Name: spec.Name, Tag: tag})
	}
	var body strings.Builder
	if err := s.index.Execute(&body, data); err != nil {
		s.write(w, http.StatusInternalServerError, "text/plain; charset=utf-8", []byte("render error\n"))
		return
	}
	s.write(w, http.StatusOK, "text/html; charset=utf-8", []byte(body.String()))
}

func (s *appServer) serveAllM3U(w http.ResponseWriter, r *http.Request) {
	host := s.publicHost(r)
	var body strings.Builder
	body.WriteString("#EXTM3U\n")
	for _, spec := range channelSpecs {
		fmt.Fprintf(&body, "#EXTINF:-1 group-title=\"%s\",%s\nhttp://%s/%s.m3u8\n", channelGroup(spec), spec.Name, host, spec.Slug)
	}
	for _, source := range galaSources {
		fmt.Fprintf(&body, "#EXTINF:-1 group-title=\"春晚\",%s\nhttp://%s/vod/spring-gala/%s.mp4\n", source.Name, host, source.Slug)
	}
	s.write(w, http.StatusOK, "application/vnd.apple.mpegurl", []byte(body.String()))
}

func channelGroup(spec channelSpec) string {
	switch {
	case strings.HasPrefix(spec.Slug, "cctv"):
		return "中央台"
	case strings.HasPrefix(spec.Slug, "cgtn"):
		return "国际台"
	case strings.HasSuffix(spec.Slug, "ws"):
		return "地方台"
	default:
		return "其他"
	}
}

func (s *appServer) serveDiagnostics(w http.ResponseWriter) {
	var body strings.Builder
	fmt.Fprintf(&body, "ysp-live-go version=%s\n", version)
	var deviceState deviceDiagnostics
	if s.device == nil {
		body.WriteString("内置设备协议: 已禁用\n")
	} else {
		deviceState = s.device.Diagnostics()
		lastError := s.device.LastError()
		switch {
		case s.device.Ready():
			body.WriteString("内置设备协议: 会话就绪")
		case lastError != "":
			body.WriteString("内置设备协议: 会话初始化失败")
		default:
			body.WriteString("内置设备协议: 等待首次使用")
		}
		if lastError != "" {
			fmt.Fprintf(&body, "（%s）", lastError)
		}
		body.WriteByte('\n')
		s.deviceMu.Lock()
		retryAt := s.deviceRetryAt
		s.deviceMu.Unlock()
		if time.Now().Before(retryAt) {
			fmt.Fprintf(&body, "device_bootstrap_retry_at=%s\n", retryAt.UTC().Format(time.RFC3339))
		}
		for _, attempt := range deviceState.Cloud {
			fmt.Fprintf(&body, "device_cloud stage=%s at=%s http=%d result=%d guid=%t shape=%s err=%q\n",
				attempt.Stage, attempt.At.UTC().Format(time.RFC3339), attempt.HTTP, attempt.Result,
				attempt.HasGUID, attempt.Shape, attempt.Error)
		}
	}
	now := time.Now()
	for _, item := range s.channels.diagnostics() {
		age := "从未成功"
		if !item.LastOK.IsZero() {
			age = fmt.Sprintf("%d秒前", int(now.Sub(item.LastOK).Seconds()))
		}
		progress := "从未更新"
		if !item.LastProgress.IsZero() {
			progress = fmt.Sprintf("%d秒前", int(now.Sub(item.LastProgress).Seconds()))
		}
		fmt.Fprintf(&body, "%s mode=%s source=%s 最后成功=%s 分片更新=%s refresh_ms=%d rate_limited=%d worker=%t fresh=%t err=%s\n",
			item.Slug, item.Mode, item.SourceHost, age, progress, item.LastRefresh.Milliseconds(), item.RateLimited,
			item.WorkerRunning, item.Fresh, item.LastError)
		if item.BKStatus != "" || item.JCEStatus != "" || deviceState.Errors[item.Slug] != "" {
			fmt.Fprintf(&body, "  %s bk=%q jce=%q device_err=%q\n", item.Slug, item.BKStatus, item.JCEStatus, deviceState.Errors[item.Slug])
		}
	}
	s.write(w, http.StatusOK, "text/plain; charset=utf-8", []byte(body.String()))
}

func (s *appServer) serveChannel(w http.ResponseWriter, r *http.Request, slug string) {
	s.serveChannelWithBudget(w, r, slug, 75*time.Second, deviceRefreshTimeout)
}

func (s *appServer) serveChannelWithBudget(w http.ResponseWriter, r *http.Request, slug string, totalBudget, deviceBudget time.Duration) {
	ch := s.channels.channels[slug]
	if ch == nil {
		s.write(w, http.StatusNotFound, "text/plain; charset=utf-8", []byte("未知频道\n"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), totalBudget)
	defer cancel()
	deviceFallback := s.device != nil && deviceChannels[slug]
	primaryCtx := ctx
	primaryCancel := func() {}
	if deviceFallback && s.deviceAttemptAllowed() {
		// Reserve time for the device fallback within the same request budget.
		primaryCtx, primaryCancel = context.WithTimeout(ctx, max(0, totalBudget-deviceBudget))
	}
	err := s.channels.ensure(primaryCtx, ch)
	primaryCancel()
	playlist := s.channels.buildPlaylist(ch)
	if playlist != "" {
		s.write(w, http.StatusOK, "application/vnd.apple.mpegurl", []byte(playlist))
		return
	}

	// Only try the device source when neither BK nor JCE has a fresh playlist.
	// A working primary source must not trigger device registration or warm-up.
	if deviceFallback && s.deviceAttemptAllowed() {
		deviceCtx, deviceCancel := context.WithTimeout(ctx, deviceBudget)
		devicePlaylist, deviceErr := s.device.FetchPlaylist(deviceCtx, slug, s.publicHost(r))
		if deviceCtx.Err() == nil {
			s.recordDeviceAttempt(deviceErr)
		}
		deviceCancel()
		// Prefer a primary source that recovered while the device request was
		// in flight, without committing the unused device timeline.
		if playlist = s.channels.buildPlaylist(ch); playlist != "" {
			s.write(w, http.StatusOK, "application/vnd.apple.mpegurl", []byte(playlist))
			return
		}
		if deviceErr == nil {
			ch.mu.Lock()
			devicePlaylist = ch.output.normalizeWithIdentity(devicePlaylist, "device-go", s.device.MediaIdentity)
			servedSource := ch.output.source
			ch.mu.Unlock()
			w.Header().Set("X-YSP-Source", servedSource)
			s.write(w, http.StatusOK, "application/vnd.apple.mpegurl", []byte(devicePlaylist))
			return
		}
		if !errors.Is(deviceErr, errDeviceLinkQuota) && !errors.Is(deviceErr, errDeviceCoolingDown) && !errors.Is(deviceErr, context.Canceled) {
			log.Printf("BK/JCE unavailable; built-in device %s fetch failed: %v", slug, deviceErr)
		}
	}
	// The shared BK/JCE refresh may have recovered while the device attempt
	// was running. Recheck its current cache before returning an error.
	if playlist = s.channels.buildPlaylist(ch); playlist != "" {
		s.write(w, http.StatusOK, "application/vnd.apple.mpegurl", []byte(playlist))
		return
	}
	ch.mu.Lock()
	lastError := ch.lastError
	bkStatus, jceStatus := ch.bkStatus, ch.jceStatus
	ch.mu.Unlock()
	if lastError == "" && err != nil {
		lastError = truncateError(err, 120)
	}
	if lastError == "" {
		lastError = "正在拉取"
	}
	msg := fmt.Sprintf("频道 %s 暂无数据（%s），请稍后重试\n", ch.spec.Name, lastError)
	if bkStatus != "" {
		msg += "BK: " + bkStatus + "\n"
	}
	if jceStatus != "" {
		msg += "JCE: " + jceStatus + "\n"
	}
	if deviceFallback {
		deviceError := s.device.Diagnostics().Errors[slug]
		if deviceError == "" {
			deviceError = s.device.LastError()
		}
		if deviceError != "" {
			msg += "设备源: " + deviceError + "\n"
		}
	}
	w.Header().Set("Retry-After", "3")
	s.write(w, http.StatusServiceUnavailable, "text/plain; charset=utf-8", []byte(msg))
}

func (s *appServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		s.write(w, http.StatusMethodNotAllowed, "text/plain; charset=utf-8", []byte("method not allowed\n"))
		return
	}
	path := r.URL.Path
	switch path {
	case "/", "/index.html":
		s.serveIndex(w)
	case "/health":
		s.write(w, http.StatusOK, "text/plain; charset=utf-8", []byte("ok\n"))
	case "/all.m3u":
		s.serveAllM3U(w, r)
	case "/diag":
		s.serveDiagnostics(w)
	case "/vod/2023-spring-festival.mp4":
		if source, ok := galaSourceBySlug("2023"); ok {
			s.serveGala(w, r, source)
		} else {
			s.write(w, http.StatusNotFound, "text/plain; charset=utf-8", []byte("not found\n"))
		}
	default:
		if strings.HasPrefix(path, "/_device/media/") {
			if s.device == nil {
				s.write(w, http.StatusNotFound, "text/plain; charset=utf-8", []byte("not found\n"))
				return
			}
			s.device.ServeMedia(w, r)
			return
		}
		if strings.HasPrefix(path, "/vod/spring-gala/") && strings.HasSuffix(path, ".mp4") {
			slug := strings.TrimSuffix(strings.TrimPrefix(path, "/vod/spring-gala/"), ".mp4")
			if source, ok := galaSourceBySlug(slug); ok {
				s.serveGala(w, r, source)
				return
			}
			s.write(w, http.StatusNotFound, "text/plain; charset=utf-8", []byte("未知春晚节目\n"))
			return
		}
		if !strings.HasPrefix(path, "/") || !strings.HasSuffix(path, ".m3u8") {
			s.write(w, http.StatusNotFound, "text/plain; charset=utf-8", []byte("not found\n"))
			return
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(path, "/"), ".m3u8")
		if slug == "" || strings.ContainsAny(slug, "/\\.") {
			s.write(w, http.StatusNotFound, "text/plain; charset=utf-8", []byte("not found\n"))
			return
		}
		s.serveChannel(w, r, slug)
	}
}

func (s *appServer) serveGala(w http.ResponseWriter, r *http.Request, source galaSource) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	playURL, err := s.gala.resolve(ctx, source)
	if err != nil {
		log.Printf("Spring Festival Gala %d source %s failed: %v", source.Year, source.Slug, err)
		s.write(w, http.StatusBadGateway, "text/plain; charset=utf-8", []byte("上游点播地址暂时不可用，请稍后重试\n"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, playURL, http.StatusFound)
}
