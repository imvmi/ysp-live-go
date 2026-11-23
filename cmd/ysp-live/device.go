package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	deviceAK               = "9f5c54c4ed0e50109b800f7e28fec205"
	deviceAppVersion       = "1.4.1"
	deviceAppChannel       = "dangbei"
	deviceLiveUserID       = "BAEBFF2B-C516-4F34-ABC0-A824A6461CBD"
	deviceName             = "央视频电视投屏助手"
	deviceReportAppKey     = "1178c84d-4818-44ff-b415-02106e87e144"
	deviceCloudGetURL      = "https://ytpcloudws.cctv.cn/cloudps/wssapi/device/v2/get"
	deviceCloudRegisterURL = "https://ytpcloudws.cctv.cn/cloudps/wssapi/device/v2/register"
	deviceAppStartURL      = "https://ytpaddr.cctv.cn/gsnw/api/app/start/v1/01"
	deviceReportURL        = "https://ytpdata.cctv.cn/das/app/data/message/single"
	deviceLive01URL        = "https://ytpaddr.cctv.cn/gsnw/api/live/v1/01"
	deviceLive02URL        = "https://ytpaddr.cctv.cn/gsnw/api/live/v1/02"
	deviceVDNURL           = "https://ytpvdn.cctv.cn/cctvmobileinf/rest/cctv/videoliveUrl/getstream"
	deviceSessionTTL       = 2 * time.Hour
	deviceEntryTTL         = time.Minute
	devicePlaylistTTL      = 2 * time.Second
	deviceMediaLeaseTTL    = 2 * time.Minute
	deviceLinkIdleTimeout  = 5 * time.Minute
	deviceMaxLinks         = 6
)

var (
	errDeviceLinkQuota   = errors.New("device link quota reached")
	errDeviceCoolingDown = errors.New("device channel is cooling down")
)

const deviceRSAPublicKey = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAkKeLy4ywWLSnBkwR
yqYgF3HMIj05V5uuh5HjyEsZOWnu1NHu3jPQv3sr32wwQNYv5qapsNXmNgLU
DHtgHZxqPQAYXltjSRc0qhcD286t62wOIHId8zXS3s1Jy4rgU4qjQWzI9rp/
1sE0pMsmwTaJa4zuJ5iz8VwF8Av5oJ1k+HxY+/HLnjNlW1hmWLpuDYmkZYu
AoTHa1VGeHQh9FEKI8ZcL3GTQphShUoC+Kg3P1hGUVTtCYapmzPS5lkAdweb
uzwvTCfGiTErYZCnPBUSeV7BVlgjtLYIi29KvF0a8FHsJMfe/UdHcyW/Rihs
IYOtDQcRRpFGXyPXbVrzFJse24QIDAQAB
-----END PUBLIC KEY-----`

var deviceLiveIDs = map[string]string{
	"cctv1": "Live1717729995180256", "cctv2": "Live1718261577870260",
	"cctv3": "Live1718261955077261", "cctv4": "Live1718276148119264",
	"cctv5": "Live1719474204987287", "cctv5p": "Live1719473996025286",
	"cctv7": "Live1718276412224269", "cctv8": "Live1718276458899270",
	"cctv9": "Live1718276503187272", "cctv10": "Live1718276550002273",
	"cctv11": "Live1718276603690275", "cctv12": "Live1718276623932276",
	"cctv13": "Live1718276575708274", "cctv14": "Live1718276498748271",
	"cctv15": "Live1718276319614267", "cctv16": "Live1718276256572265",
	"cctv17": "Live1718276138318263", "cctv4k": "Live1767871224782105",
	"cctv8k": "Live1688400593818102", "cctv164k": "Live1704966749996185",
	"cgtn": "Live1719392219423280", "cgtnfr": "Live1719392670442283",
	"cgtnru": "Live1719392779653284", "cgtnar": "Live1719392885692285",
	"cgtnes": "Live1719392560433282", "cgtndoc": "Live1719392360336281",
}

type deviceProfile struct {
	AndroidID    string `json:"android_id"`
	MAC          string `json:"mac"`
	Hardware     string `json:"hardware"`
	Board        string `json:"board"`
	Brand        string `json:"brand"`
	Device       string `json:"device"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Product      string `json:"product"`
	Tags         string `json:"tags"`
	BuildType    string `json:"build_type"`
	User         string `json:"user"`
	Resolution   string `json:"resolution"`
	Display      string `json:"display"`
	VersionID    string `json:"version_id"`
	Host         string `json:"host"`
	Fingerprint  string `json:"fingerprint"`
	ReportModel  string `json:"report_model"`
}

type deviceState struct {
	SchemaVersion int           `json:"schema_version"`
	ProfileSource string        `json:"profile_source"`
	Profile       deviceProfile `json:"profile"`
	ScreenParam   string        `json:"screen_param"`
	CastModel     string        `json:"cast_model"`
	XUID          string        `json:"x_uid"`
	CloudGUID     string        `json:"cloud_guid"`
	RegisteredAt  float64       `json:"registered_at"`
	UpdatedAt     float64       `json:"updated_at"`
}

type deviceIdentity struct {
	XUID        string
	Fingerprint string
	TimestampMS int64
	UID         string
}

type deviceSession struct {
	Identity          deviceIdentity
	Profile           deviceProfile
	SessionKey        string
	CloudGUID         string
	CreatedAt         time.Time
	Heartbeats        uint64
	HeartbeatFailures uint64
}

type deviceEntry struct {
	ID           string
	Slug         string
	FinalURL     string
	Headers      http.Header
	RefreshedAt  time.Time
	ExpiresAt    time.Time
	Playlist     string
	PlaylistAt   time.Time
	PlaylistBase string
	PlaylistKey  string
	Rate         string
	RateName     string
	MediaLeaseAt time.Time
	SessionUntil time.Time
}

type deviceHTTPError struct {
	Stage  string
	Status int
	Body   string
}

func (e *deviceHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s HTTP %d", e.Stage, e.Status)
	}
	return fmt.Sprintf("%s HTTP %d: %s", e.Stage, e.Status, e.Body)
}

type deviceFailure struct {
	At    time.Time
	Count int
}

type deviceEndpoints struct {
	CloudGet, CloudRegister string
	AppStart, Report        string
	Live01, Live02, VDN     string
}

type deviceSource interface {
	Start(context.Context)
	Close()
	Ready() bool
	LastError() string
	FetchPlaylist(context.Context, string, string) (string, error)
	ServeMedia(http.ResponseWriter, *http.Request)
}

type deviceManager struct {
	client        *http.Client
	genericClient *http.Client
	mediaClient   *http.Client
	statePath     string
	endpoints     deviceEndpoints

	mu           sync.Mutex
	session      *deviceSession
	entries      map[string]*deviceEntry
	mediaEntries map[string]*deviceEntry
	linked       map[string]struct{}
	lastAccess   map[string]time.Time
	failures     map[string]deviceFailure
	lastErr      string
	nextEntryID  uint64
	ctx          context.Context
	cancel       context.CancelFunc

	controlMu       sync.Mutex
	lastControl     time.Time
	controlInterval time.Duration
	tokenKey        []byte
}

func newDeviceManager(base *http.Client, statePath string) *deviceManager {
	sessionJar, _ := cookiejar.New(nil)
	genericJar, _ := cookiejar.New(nil)
	controlClient := &http.Client{Transport: base.Transport, Timeout: 15 * time.Second, Jar: sessionJar}
	genericClient := &http.Client{Transport: base.Transport, Timeout: 15 * time.Second, Jar: genericJar}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		sum := sha256.Sum256([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
		key = sum[:]
	}
	return &deviceManager{
		client:        controlClient,
		genericClient: genericClient,
		mediaClient:   &http.Client{Transport: base.Transport},
		statePath:     statePath,
		endpoints: deviceEndpoints{
			CloudGet: deviceCloudGetURL, CloudRegister: deviceCloudRegisterURL,
			AppStart: deviceAppStartURL, Report: deviceReportURL,
			Live01: deviceLive01URL, Live02: deviceLive02URL, VDN: deviceVDNURL,
		},
		entries:         make(map[string]*deviceEntry),
		mediaEntries:    make(map[string]*deviceEntry),
		linked:          make(map[string]struct{}),
		lastAccess:      make(map[string]time.Time),
		failures:        make(map[string]deviceFailure),
		controlInterval: time.Second,
		tokenKey:        key,
	}
}

func (m *deviceManager) Start(parent context.Context) {
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return
	}
	m.ctx, m.cancel = context.WithCancel(parent)
	ctx := m.ctx
	m.mu.Unlock()
	go m.heartbeatLoop(ctx)
}

func (m *deviceManager) Close() {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *deviceManager) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session != nil && time.Since(m.session.CreatedAt) < deviceSessionTTL
}

func (m *deviceManager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

func (m *deviceManager) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.controlMu.Lock()
			m.mu.Lock()
			session := m.session
			m.mu.Unlock()
			if session != nil && time.Since(session.CreatedAt) < deviceSessionTTL-5*time.Minute {
				err := m.heartbeat(ctx, session)
				m.mu.Lock()
				if err != nil {
					m.lastErr = "device heartbeat: " + err.Error()
					if m.session == session {
						session.HeartbeatFailures++
						if shouldInvalidateDeviceSession(err) {
							m.invalidateSessionLocked(session)
						}
					}
				} else if m.session == session {
					session.Heartbeats++
					session.HeartbeatFailures = 0
				}
				m.mu.Unlock()
			}
			m.controlMu.Unlock()
		}
	}
}

func (m *deviceManager) FetchPlaylist(ctx context.Context, slug, publicHost string) (string, error) {
	if deviceLiveIDs[slug] == "" {
		return "", errors.New("channel is not supported by the built-in device protocol")
	}
	now := time.Now()
	m.mu.Lock()
	m.cleanupMediaEntriesLocked(now)
	m.evictIdleLinksLocked(now)
	if failure, ok := m.failures[slug]; ok {
		delay := 15 * time.Second
		if failure.Count == 2 {
			delay = time.Minute
		} else if failure.Count >= 3 {
			delay = 2 * time.Minute
		}
		if now.Sub(failure.At) < delay {
			m.mu.Unlock()
			return "", errDeviceCoolingDown
		}
	}
	if err := m.reserveLinkLocked(slug, now); err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.lastAccess[slug] = now
	entry := m.entries[slug]
	if entry != nil && now.Before(entry.ExpiresAt) && now.Sub(entry.PlaylistAt) < devicePlaylistTTL {
		playlist, base, entryID := entry.Playlist, entry.PlaylistBase, entry.ID
		m.mu.Unlock()
		return rewriteDevicePlaylist(playlist, base, publicHost, entryID, m.signMediaURL), nil
	}
	needsResolve := entry == nil || now.After(entry.ExpiresAt)
	m.mu.Unlock()

	if needsResolve {
		var err error
		entry, err = m.resolveEntry(ctx, slug)
		if err != nil {
			m.recordFailure(slug, err)
			return "", err
		}
	}
	playlist, base, err := m.fetchUpstreamPlaylist(ctx, entry)
	if err != nil {
		m.recordFailure(slug, err)
		return "", err
	}
	m.mu.Lock()
	now = time.Now()
	key := playlistMediaKey(playlist)
	entry.Playlist, entry.PlaylistAt, entry.PlaylistBase = playlist, now, base
	entry.MediaLeaseAt = now.Add(deviceMediaLeaseTTL)
	if key != "" && key != entry.PlaylistKey {
		entry.PlaylistKey = key
		entry.ExpiresAt = minTime(now.Add(deviceEntryTTL), entry.SessionUntil)
	}
	delete(m.failures, slug)
	m.lastErr = ""
	m.mu.Unlock()
	return rewriteDevicePlaylist(playlist, base, publicHost, entry.ID, m.signMediaURL), nil
}

func (m *deviceManager) evictIdleLinksLocked(now time.Time) {
	for slug := range m.linked {
		if last := m.lastAccess[slug]; last.IsZero() || now.Sub(last) > deviceLinkIdleTimeout {
			delete(m.linked, slug)
		}
	}
}

func (m *deviceManager) reserveLinkLocked(slug string, now time.Time) error {
	if _, linked := m.linked[slug]; linked {
		return nil
	}
	if len(m.linked) >= deviceMaxLinks {
		return fmt.Errorf("%w (%d)", errDeviceLinkQuota, deviceMaxLinks)
	}
	m.linked[slug] = struct{}{}
	m.lastAccess[slug] = now
	return nil
}

func (m *deviceManager) cleanupMediaEntriesLocked(now time.Time) {
	for id, entry := range m.mediaEntries {
		if now.After(entry.MediaLeaseAt) {
			delete(m.mediaEntries, id)
		}
	}
}

func (m *deviceManager) recordFailure(slug string, err error) {
	m.mu.Lock()
	failure := m.failures[slug]
	failure.At, failure.Count = time.Now(), failure.Count+1
	m.failures[slug] = failure
	m.lastErr = slug + ": " + err.Error()
	m.mu.Unlock()
}

func (m *deviceManager) resolveEntry(ctx context.Context, slug string) (*deviceEntry, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.mu.Lock()
	if entry := m.entries[slug]; entry != nil && time.Now().Before(entry.ExpiresAt) {
		m.mu.Unlock()
		return entry, nil
	}
	m.mu.Unlock()
	session, err := m.ensureSessionLocked(ctx)
	if err != nil {
		return nil, err
	}
	// A newly bootstrapped session clears the previous session's link table.
	// Re-register the channel that triggered the bootstrap before asking the
	// upstream for a new link.
	m.mu.Lock()
	now := time.Now()
	m.evictIdleLinksLocked(now)
	err = m.reserveLinkLocked(slug, now)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	liveURL, rate, rateName, err := m.live01(ctx, session, deviceLiveIDs[slug])
	if err != nil {
		if shouldInvalidateDeviceSession(err) {
			m.invalidateSession(session)
		}
		return nil, err
	}
	secret, err := m.live02(ctx, session)
	if err != nil {
		if shouldInvalidateDeviceSession(err) {
			m.invalidateSession(session)
		}
		return nil, err
	}
	finalURL, appSign, randomString, err := m.vdn(ctx, session, liveURL, secret)
	if err != nil {
		if shouldInvalidateDeviceSession(err) {
			m.invalidateSession(session)
		}
		return nil, err
	}
	headers := http.Header{
		"UID":          {session.Profile.AndroidID},
		"APPID":        {deviceAK},
		"APPSIGN":      {appSign},
		"APPRANDOMSTR": {randomString},
		"Referer":      {"api.cctv.cn"},
		"User-Agent":   {"cctv_app_tv"},
	}
	now = time.Now()
	m.mu.Lock()
	m.nextEntryID++
	entry := &deviceEntry{
		ID: slug + "-" + strconv.FormatUint(m.nextEntryID, 36), Slug: slug,
		FinalURL: finalURL, Headers: headers, RefreshedAt: now,
		ExpiresAt:    minTime(now.Add(deviceEntryTTL), session.CreatedAt.Add(deviceSessionTTL)),
		MediaLeaseAt: now.Add(deviceMediaLeaseTTL), SessionUntil: session.CreatedAt.Add(deviceSessionTTL),
		Rate: rate, RateName: rateName,
	}
	m.entries[slug] = entry
	m.mediaEntries[entry.ID] = entry
	m.mu.Unlock()
	return entry, nil
}

func (m *deviceManager) ensureSessionLocked(ctx context.Context) (*deviceSession, error) {
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()
	if session != nil && time.Since(session.CreatedAt) < deviceSessionTTL-5*time.Minute {
		return session, nil
	}
	state, err := loadOrCreateDeviceState(m.statePath)
	if err != nil {
		return nil, err
	}
	identity := buildDeviceIdentity(state.Profile, time.Now())
	sessionKey, identity, err := m.appStart(ctx, state.Profile, identity)
	if err != nil {
		return nil, err
	}
	guid, err := m.cloudRegistration(ctx, identity)
	if err != nil {
		return nil, err
	}
	if guid == "" {
		guid = state.CloudGUID
	}
	if guid == "" {
		return nil, errors.New("device cloud registration returned an empty guid")
	}
	session = &deviceSession{Identity: identity, Profile: state.Profile, SessionKey: sessionKey, CloudGUID: guid, CreatedAt: time.Now()}
	if err := m.heartbeat(ctx, session); err != nil {
		return nil, err
	}
	session.Heartbeats = 1
	state.XUID, state.CloudGUID, state.UpdatedAt = identity.XUID, guid, float64(time.Now().UnixNano())/1e9
	if state.RegisteredAt == 0 {
		state.RegisteredAt = state.UpdatedAt
	}
	if err := saveDeviceState(m.statePath, state); err != nil {
		// The live session is still usable in memory when persistent storage is
		// read-only. Surface the persistence error in diagnostics only.
		m.mu.Lock()
		m.lastErr = "persist device identity: " + err.Error()
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.session = session
	clear(m.entries)
	clear(m.linked)
	m.mu.Unlock()
	return session, nil
}

func (m *deviceManager) invalidateSession(session *deviceSession) {
	m.mu.Lock()
	m.invalidateSessionLocked(session)
	m.mu.Unlock()
}

// Caller holds m.mu. Existing media entries are deliberately retained for a
// short lease so a session rollover cannot break segments already advertised
// in a player's playlist.
func (m *deviceManager) invalidateSessionLocked(session *deviceSession) {
	if m.session != session {
		return
	}
	m.session = nil
	clear(m.entries)
	clear(m.linked)
}

func shouldInvalidateDeviceSession(err error) bool {
	var httpErr *deviceHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden {
		return true
	}
	// live/v1/01 also uses HTTP 400 for the per-device link limit. Rebuilding
	// the identity in that case turns a local capacity event into a cloud
	// registration storm. A 400 from later session-bound steps is different:
	// their encrypted inputs are derived from the current session and a retry
	// with that same session cannot recover.
	return httpErr.Status == http.StatusBadRequest && httpErr.Stage != "live/v1/01"
}

func minTime(a, b time.Time) time.Time {
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}

func (m *deviceManager) waitControl(ctx context.Context) error {
	wait := m.controlInterval - time.Since(m.lastControl)
	if m.lastControl.IsZero() || wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *deviceManager) controlRequest(ctx context.Context, method, endpoint string, headers http.Header, body []byte) ([]byte, int, error) {
	return m.controlRequestWithClient(ctx, m.client, method, endpoint, headers, body)
}

func (m *deviceManager) genericRequest(ctx context.Context, method, endpoint string, headers http.Header, body []byte) ([]byte, int, error) {
	return m.controlRequestWithClient(ctx, m.genericClient, method, endpoint, headers, body)
}

func (m *deviceManager) controlRequestWithClient(ctx context.Context, client *http.Client, method, endpoint string, headers http.Header, body []byte) ([]byte, int, error) {
	if err := m.waitControl(ctx); err != nil {
		return nil, 0, err
	}
	defer func() { m.lastControl = time.Now() }()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header = headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := readLimited(resp.Body, 4<<20)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

func (m *deviceManager) appStart(ctx context.Context, profile deviceProfile, identity deviceIdentity) (string, deviceIdentity, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		now := time.Now()
		identity = buildDeviceIdentity(profile, now)
		body := map[string]any{"key": "app_start_d1", "value": buildDeviceReport(profile, identity.XUID, "", now)}
		payload, err := marshalDeviceJSON(body, true)
		if err != nil {
			return "", identity, err
		}
		headers := freshDeviceHeaders(identity, "application/json", "application/json", identity.TimestampMS, profile.AndroidID)
		headers.Set("UID", "")
		data, status, err := m.controlRequest(ctx, http.MethodPost, m.endpoints.AppStart, headers, payload)
		if err == nil && status/100 == 2 {
			var root map[string]any
			if json.Unmarshal(data, &root) == nil {
				encrypted := nestedString(root["data"], "key")
				if encrypted != "" {
					plain, decryptErr := decryptDeviceValue(encrypted, identity.Fingerprint[:32])
					if decryptErr == nil {
						return plain, identity, nil
					}
					lastErr = decryptErr
				}
			}
		}
		if err != nil {
			lastErr = err
		} else if lastErr == nil {
			lastErr = fmt.Errorf("app/start HTTP %d: %s", status, truncateError(errors.New(string(data)), 240))
		}
	}
	return "", identity, fmt.Errorf("device app/start failed: %w", lastErr)
}

func (m *deviceManager) cloudRegistration(ctx context.Context, identity deviceIdentity) (string, error) {
	encrypted, err := encryptDeviceID(identity.XUID)
	if err != nil {
		return "", err
	}
	body := map[string]any{"device_name": deviceName, "device_id": encrypted}
	request := func(endpoint string) (map[string]any, error) {
		payload, _ := marshalDeviceJSON(body, false)
		data, status, err := m.genericRequest(ctx, http.MethodPost, endpoint, freshDeviceHeaders(identity, "application/json", "application/json", 0, ""), payload)
		if err != nil {
			return nil, err
		}
		var value map[string]any
		if err := json.Unmarshal(data, &value); err != nil {
			if status/100 != 2 {
				return nil, &deviceHTTPError{Stage: "cloud device", Status: status, Body: truncateError(errors.New(string(data)), 240)}
			}
			return nil, err
		}
		// The cloud API reports normal business states (notably result=2,
		// "registration required") with HTTP 400. Its JSON result code is the
		// protocol status and must be interpreted before the HTTP status.
		return value, nil
	}
	value, err := request(m.endpoints.CloudGet)
	if err != nil {
		return "", err
	}
	if code := resultCode(value); code == 0 {
		if guid := extractDeviceGUID(value); guid != "" {
			return guid, nil
		}
		// Some cloud responses report success for a known-but-unregistered
		// identity without returning a guid. Continue through registration
		// instead of treating the empty value as a usable session.
	} else if code != 601 && code != 2 {
		return "", fmt.Errorf("cloud device get returned result %d", code)
	}
	var code int
	for attempt := 0; attempt < 2; attempt++ {
		value, err = request(m.endpoints.CloudRegister)
		if err != nil {
			return "", err
		}
		code = resultCode(value)
		if guid := extractDeviceGUID(value); guid != "" {
			return guid, nil
		}
		if code != 695 {
			break
		}
	}
	if code == 0 || code == 694 || code == 2 {
		value, err = request(m.endpoints.CloudGet)
		if err != nil {
			return "", err
		}
		return extractDeviceGUID(value), nil
	}
	return "", fmt.Errorf("cloud device registration returned result %d", code)
}

func (m *deviceManager) heartbeat(ctx context.Context, session *deviceSession) error {
	value := buildDeviceReport(session.Profile, session.Identity.XUID, "", time.Now())
	value["network_type"], value["guid"], value["other"] = "WiFi", session.CloudGUID, ""
	payload, _ := marshalDeviceJSON(map[string]any{"key": "app_heartbeat", "value": value}, false)
	_, status, err := m.genericRequest(ctx, http.MethodPost, m.endpoints.Report, freshDeviceHeaders(session.Identity, "application/json", "application/json", 0, ""), payload)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return &deviceHTTPError{Stage: "heartbeat", Status: status}
	}
	return nil
}

func (m *deviceManager) live01(ctx context.Context, session *deviceSession, liveID string) (string, string, string, error) {
	body := map[string]any{
		"screenParam": "7680-4320-280", "rate": "", "systemType": "ios", "model": session.Profile.Model,
		"id": liveID, "userId": deviceLiveUserID, "clientSign": "cctvVideo",
		"deviceId": map[string]any{"serial": "", "imei": "", "android_id": ""},
	}
	payload, _ := marshalDeviceJSON(body, false)
	data, status, err := m.controlRequest(ctx, http.MethodPost, m.endpoints.Live01, freshDeviceHeaders(session.Identity, "application/json", "application/json", 0, ""), payload)
	if err != nil {
		return "", "", "", err
	}
	if status/100 != 2 {
		return "", "", "", &deviceHTTPError{Stage: "live/v1/01", Status: status, Body: truncateError(errors.New(string(data)), 240)}
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return "", "", "", err
	}
	dataMap, _ := root["data"].(map[string]any)
	videos, _ := dataMap["videoList"].([]any)
	if len(videos) == 0 {
		videos, _ = dataMap["videos"].([]any)
	}
	var selected map[string]any
	for _, raw := range videos {
		video, _ := raw.(map[string]any)
		if stringValue(video["url"]) == "" {
			continue
		}
		if selected == nil || stringValue(video["rate"]) == "36p" {
			selected = video
		}
		if stringValue(video["rate"]) == "36p" {
			break
		}
	}
	if selected == nil {
		return "", "", "", errors.New("live/v1/01 did not return a usable video")
	}
	liveURL := stringValue(selected["url"])
	if !strings.HasPrefix(liveURL, "http://") && !strings.HasPrefix(liveURL, "https://") {
		liveURL, err = decryptDeviceValue(liveURL, session.SessionKey)
		if err != nil {
			return "", "", "", err
		}
	}
	return liveURL, stringValue(selected["rate"]), stringValue(selected["rateName"]), nil
}

func (m *deviceManager) live02(ctx context.Context, session *deviceSession) (string, error) {
	encryptedGUID, err := encryptDeviceValue("", session.SessionKey)
	if err != nil {
		return "", err
	}
	payload, _ := marshalDeviceJSON(map[string]any{"guid": encryptedGUID}, false)
	data, status, err := m.controlRequest(ctx, http.MethodPost, m.endpoints.Live02, freshDeviceHeaders(session.Identity, "application/json", "application/json", 0, ""), payload)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		return "", &deviceHTTPError{Stage: "live/v1/02", Status: status, Body: truncateError(errors.New(string(data)), 240)}
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return "", err
	}
	encrypted := nestedString(root["data"], "appSecret", "app_secret")
	if encrypted == "" {
		return "", errors.New("live/v1/02 did not return appSecret")
	}
	return decryptDeviceValue(encrypted, session.SessionKey)
}

func (m *deviceManager) vdn(ctx context.Context, session *deviceSession, liveURL, secret string) (string, string, string, error) {
	randomString, err := randomVDNString()
	if err != nil {
		return "", "", "", err
	}
	signRaw := md5.Sum([]byte(deviceAK + secret + randomString))
	appSign := hex.EncodeToString(signRaw[:])
	appCommonBytes, _ := marshalDeviceJSON(map[string]any{"adid": "", "av": deviceAppVersion, "an": deviceName, "ap": "cctv_app_tv"}, false)
	form := url.Values{"appcommon": {string(appCommonBytes)}, "url": {liveURL}}
	headers := freshDeviceHeaders(session.Identity, "application/x-www-form-urlencoded", "", 0, "")
	headers.Del("Accept")
	headers.Set("APPID", deviceAK)
	headers.Set("APPSIGN", appSign)
	headers.Set("APPRANDOMSTR", randomString)
	data, status, err := m.genericRequest(ctx, http.MethodPost, m.endpoints.VDN, headers, []byte(form.Encode()))
	if err != nil {
		return "", "", "", err
	}
	if status/100 != 2 {
		return "", "", "", &deviceHTTPError{Stage: "VDN", Status: status, Body: truncateError(errors.New(string(data)), 240)}
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return "", "", "", err
	}
	if fmt.Sprint(root["succeed"]) != "1" {
		return "", "", "", fmt.Errorf("VDN did not return a stream URL: %s", truncateError(errors.New(string(data)), 320))
	}
	finalURL := stringValue(root["url"])
	if finalURL == "" {
		return "", "", "", errors.New("VDN returned an empty stream URL")
	}
	return finalURL, appSign, randomString, nil
}

func (m *deviceManager) fetchUpstreamPlaylist(ctx context.Context, entry *deviceEntry) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.FinalURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header = entry.Headers.Clone()
	resp, err := m.mediaClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", "", fmt.Errorf("device playlist HTTP %d", resp.StatusCode)
	}
	body, err := readLimited(resp.Body, 16<<20)
	if err != nil {
		return "", "", err
	}
	playlist := string(body)
	if err := validateMediaPlaylist(playlist); err != nil {
		return "", "", err
	}
	return playlist, resp.Request.URL.String(), nil
}

func rewriteDevicePlaylist(playlist, base, publicHost, entryID string, signer func(string, string) string) string {
	baseURL, err := url.Parse(base)
	if err != nil || publicHost == "" {
		return playlist
	}
	var out strings.Builder
	for _, line := range strings.Split(playlist, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			if ref, parseErr := url.Parse(trimmed); parseErr == nil {
				realURL := baseURL.ResolveReference(ref).String()
				line = "http://" + publicHost + "/_device/media/" + signer(entryID, realURL)
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

func (m *deviceManager) signMediaURL(entryID, realURL string) string {
	payload := []byte(entryID + "\x00" + realURL)
	mac := hmac.New(sha256.New, m.tokenKey)
	_, _ = mac.Write(payload)
	tag := mac.Sum(nil)[:16]
	token := append(append([]byte{}, payload...), tag...)
	return base64.RawURLEncoding.EncodeToString(token)
}

func (m *deviceManager) decodeMediaURL(token string) (string, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < 17 {
		return "", "", errors.New("invalid device media token")
	}
	payload, tag := raw[:len(raw)-16], raw[len(raw)-16:]
	mac := hmac.New(sha256.New, m.tokenKey)
	_, _ = mac.Write(payload)
	if !hmac.Equal(tag, mac.Sum(nil)[:16]) {
		return "", "", errors.New("invalid device media signature")
	}
	parts := bytes.SplitN(payload, []byte{0}, 2)
	if len(parts) != 2 {
		return "", "", errors.New("invalid device media payload")
	}
	return string(parts[0]), string(parts[1]), nil
}

func (m *deviceManager) ServeMedia(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/_device/media/")
	entryID, realURL, err := m.decodeMediaURL(token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	now := time.Now()
	m.cleanupMediaEntriesLocked(now)
	entry := m.mediaEntries[entryID]
	if entry != nil {
		m.lastAccess[entry.Slug] = now
	}
	m.mu.Unlock()
	if entry == nil {
		http.Error(w, "device media session expired", http.StatusGone)
		return
	}
	parsed, err := url.Parse(realURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		http.Error(w, "invalid device media URL", http.StatusBadRequest)
		return
	}
	mediaCtx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(mediaCtx, r.Method, realURL, nil)
	if err != nil {
		http.Error(w, "invalid device media request", http.StatusBadGateway)
		return
	}
	req.Header = entry.Headers.Clone()
	for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	resp, err := m.mediaClient.Do(req)
	if err != nil {
		http.Error(w, "device media unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified", "Cache-Control"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.CopyBuffer(w, resp.Body, make([]byte, 128<<10))
	}
}

func freshDeviceHeaders(identity deviceIdentity, contentType, accept string, forceTimestamp int64, uid string) http.Header {
	if forceTimestamp == 0 {
		forceTimestamp = time.Now().UnixMilli()
	}
	headers := make(http.Header)
	if accept != "" {
		headers.Set("Accept", accept)
	}
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	headers.Set("Accept-Language", "zh-CN,zh;q=0.8")
	headers.Set("Referer", "api.cctv.cn")
	headers.Set("User-Agent", "cctv_app_tv")
	if uid == "" {
		uid = identity.UID
	}
	headers.Set("UID", uid)
	headers.Set("appChannel", deviceAppChannel)
	headers.Set("X-Uid", identity.XUID)
	headers.Set("X-Fingerprint", identity.Fingerprint)
	headers.Set("X-Version", deviceAppVersion)
	headers.Set("X-Timestamp", strconv.FormatInt(forceTimestamp, 10))
	headers.Set("X-Nonce", randomUUID())
	headers.Set("Cache-Control", "no-cache")
	return headers
}

func buildDeviceIdentity(profile deviceProfile, now time.Time) deviceIdentity {
	xuid := computeDeviceXUID(profile)
	timestamp := now.UnixMilli()
	day0 := int64(86400000)*((now.Unix()+28800)/86400) - 28800000
	first := sha256.Sum256([]byte(deviceAK + xuid + strconv.FormatInt(timestamp, 10) + strconv.FormatInt(day0, 10)))
	second := sha256.Sum256([]byte(hex.EncodeToString(first[:])))
	return deviceIdentity{XUID: xuid, Fingerprint: hex.EncodeToString(second[:]), TimestampMS: timestamp, UID: profile.AndroidID}
}

func computeDeviceXUID(profile deviceProfile) string {
	build := "1698" + profile.Hardware + profile.Board + profile.Brand + profile.Device + profile.Manufacturer + profile.Model + profile.Product + profile.Tags + profile.BuildType + profile.User + profile.Resolution + profile.MAC
	uuidPart := fmt.Sprintf("%016x%016x", uint64(int64(javaHash(build))), uint64(int64(javaHash(profile.Model))))
	sum := sha1.Sum([]byte(profile.AndroidID + "|" + uuidPart))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func javaHash(value string) int32 {
	var hash int32
	for _, r := range value {
		hash = hash*31 + int32(r)
	}
	return hash
}

func buildDeviceReport(profile deviceProfile, xuid, sdkVersion string, now time.Time) map[string]any {
	return map[string]any{
		"cctv_id": xuid, "device_id": profile.AndroidID, "idfa": "", "idfv": "", "user_id": "",
		"app_key": deviceReportAppKey, "imei": "", "android_id": profile.AndroidID, "mac": profile.MAC,
		"device_builder_type": profile.BuildType, "device_hardware": profile.Hardware, "device_board": profile.Board,
		"device_brand": profile.Brand, "device_params": profile.Device, "device_display": profile.Display,
		"device_version_id": profile.VersionID, "device_host": profile.Host, "device_product": profile.Product,
		"device_tags": profile.Tags, "device_user": profile.User, "device_fingerprint": profile.Fingerprint,
		"device_manufacturer": profile.Manufacturer, "device_model": profile.ReportModel, "device_resolution": profile.Resolution,
		"system_type": "Android", "device_type": "TV", "app_language": "CHINESE", "app_version": deviceAppVersion,
		"sdk_version": sdkVersion, "os_version": "13", "app_channel": deviceAppChannel,
		"data_time": strconv.FormatInt(now.UnixMilli(), 10),
	}
}

func marshalDeviceJSON(value any, escapeSlashTwice bool) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil || !escapeSlashTwice {
		return data, err
	}
	data = bytes.ReplaceAll(data, []byte("/"), []byte(`\/`))
	return data, nil
}

func normalizeDeviceKey(value string) []byte {
	key := make([]byte, 32)
	copy(key, []byte(value))
	return key
}

func decryptDeviceValue(value, key string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) <= 12 {
		return "", errors.New("invalid encrypted device value")
	}
	block, err := aes.NewCipher(normalizeDeviceKey(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, raw[:12], raw[12:], nil)
	if err != nil {
		return "", fmt.Errorf("device AES-GCM decrypt: %w", err)
	}
	return string(plain), nil
}

func encryptDeviceValue(value, key string) (string, error) {
	block, err := aes.NewCipher(normalizeDeviceKey(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(value), nil)
	return base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func encryptDeviceID(value string) (string, error) {
	block, _ := pem.Decode([]byte(deviceRSAPublicKey))
	if block == nil {
		return "", errors.New("invalid device RSA public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", err
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("device key is not RSA")
	}
	encrypted, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, publicKey, []byte(value), nil)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func randomVDNString() (string, error) {
	raw := make([]byte, 10)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x-0000-%04x-0000-00000000%04x",
		uint32(raw[0])<<24|uint32(raw[1])<<16|uint32(raw[2])<<8|uint32(raw[3]),
		uint16(raw[4])<<8|uint16(raw[5]), uint16(raw[6])<<8|uint16(raw[7])), nil
}

func randomUUID() string {
	raw := make([]byte, 16)
	_, _ = io.ReadFull(rand.Reader, raw)
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}

func loadOrCreateDeviceState(path string) (deviceState, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		var state deviceState
		if json.Unmarshal(data, &state) == nil && len(state.Profile.AndroidID) == 16 && state.Profile.MAC != "" {
			state.XUID = computeDeviceXUID(state.Profile)
			if state.ScreenParam == "" {
				state.ScreenParam = "7680-4320-280"
			}
			if state.CastModel == "" {
				state.CastModel = state.Profile.Model
			}
			return state, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return deviceState{}, err
	}
	androidID, err := randomHex(8)
	if err != nil {
		return deviceState{}, err
	}
	macBytes := make([]byte, 6)
	if _, err := io.ReadFull(rand.Reader, macBytes); err != nil {
		return deviceState{}, err
	}
	macBytes[0] = (macBytes[0] | 2) & 0xfe
	macParts := make([]string, 6)
	for i, b := range macBytes {
		macParts[i] = fmt.Sprintf("%02x", b)
	}
	profile := deviceProfile{
		AndroidID: androidID, MAC: strings.Join(macParts, ":"), Hardware: "mt5895", Board: "mt5895",
		Brand: "Sony", Device: "sony_xr_85z9k", Manufacturer: "Sony", Model: "XR-85Z9K",
		Product: "sony_xr_85z9k", Tags: "release-keys", BuildType: "user", User: "build",
		Resolution: "7680*4320", Display: "XR-85Z9K-user 13 SONYTV.2022.XR_85Z9K 2024 release-keys",
		VersionID: "SONYTV.2022.XR_85Z9K", Host: "sony-tv-build",
		Fingerprint: "Sony/sony_xr_85z9k/sony_xr_85z9k:13/SONYTV.2022.XR_85Z9K/2024:user/release-keys",
		ReportModel: "XR85Z9K",
	}
	state := deviceState{SchemaVersion: 1, ProfileSource: "go.sony.XR-85Z9K", Profile: profile, ScreenParam: "7680-4320-280", CastModel: "XR-85Z9K", UpdatedAt: float64(time.Now().UnixNano()) / 1e9}
	state.XUID = computeDeviceXUID(profile)
	if err := saveDeviceState(path, state); err != nil {
		return state, nil
	}
	return state, nil
}

func saveDeviceState(path string, state deviceState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			_ = os.Remove(tmp)
			return err
		}
		if renameErr := os.Rename(tmp, path); renameErr == nil {
			return nil
		}
	}
	_ = os.Remove(tmp)
	return err
}

func randomHex(bytesCount int) (string, error) {
	raw := make([]byte, bytesCount)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func nestedString(value any, keys ...string) string {
	if text, ok := value.(string); ok {
		return text
	}
	mapping, _ := value.(map[string]any)
	for _, key := range keys {
		if text := stringValue(mapping[key]); text != "" {
			return text
		}
	}
	return ""
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func resultCode(value any) int {
	if mapping, ok := value.(map[string]any); ok {
		for _, key := range []string{"result", "code", "errCode", "errcode", "ret"} {
			switch raw := mapping[key].(type) {
			case float64:
				return int(raw)
			case string:
				if parsed, err := strconv.Atoi(raw); err == nil {
					return parsed
				}
			}
		}
		for _, key := range []string{"data", "error", "response"} {
			if mapping[key] != nil {
				if code := resultCode(mapping[key]); code != -1 {
					return code
				}
			}
		}
	}
	return -1
}

func extractDeviceGUID(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"guid", "deviceGuid", "device_guid"} {
			if guid := stringValue(typed[key]); guid != "" {
				return guid
			}
		}
		for _, key := range []string{"data", "response", "device"} {
			if guid := extractDeviceGUID(typed[key]); guid != "" {
				return guid
			}
		}
	case []any:
		for _, item := range typed {
			if guid := extractDeviceGUID(item); guid != "" {
				return guid
			}
		}
	}
	return ""
}
