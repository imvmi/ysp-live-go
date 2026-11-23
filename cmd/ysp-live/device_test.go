package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testDeviceProfile() deviceProfile {
	return deviceProfile{
		AndroidID: "0123456789abcdef", MAC: "02:11:22:33:44:55", Hardware: "mt5895", Board: "mt5895",
		Brand: "Sony", Device: "sony_xr_85z9k", Manufacturer: "Sony", Model: "XR-85Z9K",
		Product: "sony_xr_85z9k", Tags: "release-keys", BuildType: "user", User: "build",
		Resolution: "7680*4320", Display: "XR-85Z9K-user 13 SONYTV.2022.XR_85Z9K 2024 release-keys",
		VersionID: "SONYTV.2022.XR_85Z9K", Host: "sony-tv-build",
		Fingerprint: "Sony/sony_xr_85z9k/sony_xr_85z9k:13/SONYTV.2022.XR_85Z9K/2024:user/release-keys",
		ReportModel: "XR85Z9K",
	}
}

func TestDeviceProtocolCompatibilityVectors(t *testing.T) {
	profile := testDeviceProfile()
	if got, want := computeDeviceXUID(profile), "B98BD1238CC83FCFEAA5298FB4020D3DE9788B38"; got != want {
		t.Fatalf("X-Uid mismatch: got %s want %s", got, want)
	}
	identity := buildDeviceIdentity(profile, time.UnixMilli(1700000000123))
	if got, want := identity.Fingerprint, "e74e24dff3a5a7ec454625301a42eb828b4966a5b542f45ebaae562f009b4431"; got != want {
		t.Fatalf("fingerprint mismatch: got %s want %s", got, want)
	}
	ciphertext := "AAECAwQFBgcICQoLRYDXELY1tsnetRLbUG1KgfPcpkYq8d5qE2mdDw=="
	plain, err := decryptDeviceValue(ciphertext, "0123456789abcdef0123456789abcdef")
	if err != nil || plain != "hello-device" {
		t.Fatalf("AES-GCM vector failed: plain=%q err=%v", plain, err)
	}
	encoded, err := marshalDeviceJSON(map[string]any{"a": "x/y"}, true)
	if err != nil || string(encoded) != `{"a":"x\/y"}` {
		t.Fatalf("app/start JSON escaping mismatch: %q err=%v", encoded, err)
	}
	if encrypted, err := encryptDeviceID(identity.XUID); err != nil {
		t.Fatal(err)
	} else if raw, decodeErr := base64.StdEncoding.DecodeString(encrypted); decodeErr != nil || len(raw) != 256 {
		t.Fatalf("RSA-OAEP output invalid: bytes=%d err=%v", len(raw), decodeErr)
	}
}

func TestBuiltInDeviceCCTV10FlowAndMedia(t *testing.T) {
	const sessionKey = "0123456789abcdef0123456789abcdef"
	const appSecret = "test-app-secret"
	var appStarts atomic.Int32
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/app-start", func(w http.ResponseWriter, r *http.Request) {
		appStarts.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "session-cookie", Value: "app-start", Path: "/"})
		fingerprint := r.Header.Get("X-Fingerprint")
		if len(fingerprint) < 32 {
			http.Error(w, "missing fingerprint", http.StatusBadRequest)
			return
		}
		encrypted, err := encryptDeviceValue(sessionKey, fingerprint[:32])
		if err != nil {
			t.Fatal(err)
		}
		writeTestJSON(w, map[string]any{"data": map[string]any{"key": encrypted}})
	})
	mux.HandleFunc("/cloud-get", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			http.Error(w, "app session cookie leaked to generic client", http.StatusBadRequest)
			return
		}
		writeTestJSON(w, map[string]any{"result": 0, "data": map[string]any{"guid": "test-guid"}})
	})
	mux.HandleFunc("/cloud-register", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("registered an identity that cloud-get already knew")
	})
	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"result": 0})
	})
	mux.HandleFunc("/live01", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("session-cookie"); err != nil || cookie.Value != "app-start" {
			http.Error(w, "app session cookie was not preserved", http.StatusUnauthorized)
			return
		}
		writeTestJSON(w, map[string]any{"data": map[string]any{"videoList": []any{
			map[string]any{"url": "https://example.invalid/live", "rate": "10p", "rateName": "高码率"},
		}}})
	})
	mux.HandleFunc("/live02", func(w http.ResponseWriter, r *http.Request) {
		encrypted, err := encryptDeviceValue(appSecret, sessionKey)
		if err != nil {
			t.Fatal(err)
		}
		writeTestJSON(w, map[string]any{"data": map[string]any{"appSecret": encrypted}})
	})
	mux.HandleFunc("/vdn", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("APPID") != deviceAK || r.Header.Get("APPSIGN") == "" || r.Header.Get("APPRANDOMSTR") == "" {
			http.Error(w, "missing VDN signature", http.StatusBadRequest)
			return
		}
		writeTestJSON(w, map[string]any{"succeed": "1", "url": server.URL + "/stream.m3u8"})
	})
	mux.HandleFunc("/stream.m3u8", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:8\n#EXTINF:4,\nsegment.ts\n")
	})
	mux.HandleFunc("/segment.ts", func(w http.ResponseWriter, r *http.Request) {
		packet := append([]byte{0x47}, make([]byte, 187)...)
		if r.Header.Get("Range") == "bytes=0-187" {
			w.Header().Set("Content-Range", "bytes 0-187/376")
			w.Header().Set("Content-Length", "188")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(packet)
			return
		}
		_, _ = w.Write(append(packet, packet...))
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	manager := newDeviceManager(server.Client(), filepath.Join(t.TempDir(), "data", "device-state-go.json"))
	manager.controlInterval = 0
	manager.endpoints = deviceEndpoints{
		CloudGet: server.URL + "/cloud-get", CloudRegister: server.URL + "/cloud-register",
		AppStart: server.URL + "/app-start", Report: server.URL + "/heartbeat",
		Live01: server.URL + "/live01", Live02: server.URL + "/live02", VDN: server.URL + "/vdn",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	playlist, err := manager.FetchPlaylist(ctx, "cctv10", "127.0.0.1:8767")
	if err != nil {
		t.Fatal(err)
	}
	if !manager.Ready() || appStarts.Load() != 1 {
		t.Fatalf("built-in device session not reused: ready=%t starts=%d", manager.Ready(), appStarts.Load())
	}
	manager.mu.Lock()
	_, firstLinkTracked := manager.linked["cctv10"]
	manager.mu.Unlock()
	if !firstLinkTracked {
		t.Fatal("channel that bootstrapped the session was not counted against the link quota")
	}
	mediaURL := ""
	for _, line := range strings.Split(playlist, "\n") {
		if strings.HasPrefix(line, "http://") {
			mediaURL = line
			break
		}
	}
	parsed, err := url.Parse(mediaURL)
	if err != nil || !strings.HasPrefix(parsed.Path, "/_device/media/") || parsed.RawQuery != "" {
		t.Fatalf("invalid device media URL %q: %v", mediaURL, err)
	}
	token := strings.TrimPrefix(parsed.Path, "/_device/media/")
	replacement := "A"
	if strings.HasSuffix(token, replacement) {
		replacement = "B"
	}
	tampered := token[:len(token)-1] + replacement
	if _, _, err := manager.decodeMediaURL(tampered); err == nil {
		t.Fatal("tampered device media token was accepted")
	}
	req := httptest.NewRequest(http.MethodGet, parsed.RequestURI(), nil)
	req.Header.Set("Range", "bytes=0-187")
	recorder := httptest.NewRecorder()
	manager.ServeMedia(recorder, req)
	resp := recorder.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || len(body) != 188 || body[0] != 0x47 {
		t.Fatalf("device media status=%d bytes=%d", resp.StatusCode, len(body))
	}
	if _, err := os.Stat(manager.statePath); err != nil {
		t.Fatalf("device identity was not persisted: %v", err)
	}
	if _, err := manager.FetchPlaylist(ctx, "cctv10", "127.0.0.1:8767"); err != nil || appStarts.Load() != 1 {
		t.Fatalf("cached device session failed: starts=%d err=%v", appStarts.Load(), err)
	}
}

func TestDeviceLinkQuotaEvictsIdleChannels(t *testing.T) {
	manager := newDeviceManager(http.DefaultClient, filepath.Join(t.TempDir(), "device.json"))
	now := time.Now()
	for _, slug := range []string{"cctv1", "cctv2", "cctv3", "cctv4", "cctv5", "cctv5p"} {
		manager.linked[slug] = struct{}{}
		manager.lastAccess[slug] = now
	}
	entry := &deviceEntry{
		ID: "cctv7-test", Slug: "cctv7", ExpiresAt: now.Add(time.Minute),
		PlaylistAt: now, PlaylistBase: "https://cdn.example/live.m3u8",
		Playlist: "#EXTM3U\n#EXTINF:4,\nsegment.ts\n", MediaLeaseAt: now.Add(time.Minute),
	}
	manager.entries["cctv7"] = entry
	manager.mediaEntries[entry.ID] = entry

	_, err := manager.FetchPlaylist(context.Background(), "cctv7", "127.0.0.1:8767")
	if !errors.Is(err, errDeviceLinkQuota) {
		t.Fatalf("expected local device quota, got %v", err)
	}
	manager.mu.Lock()
	manager.lastAccess["cctv1"] = now.Add(-deviceLinkIdleTimeout - time.Second)
	manager.mu.Unlock()
	if _, err := manager.FetchPlaylist(context.Background(), "cctv7", "127.0.0.1:8767"); err != nil {
		t.Fatalf("idle link was not recycled: %v", err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.linked) != deviceMaxLinks {
		t.Fatalf("linked channels=%d, want %d", len(manager.linked), deviceMaxLinks)
	}
	if _, ok := manager.linked["cctv1"]; ok {
		t.Fatal("idle channel still occupied a device link")
	}
}

func TestDeviceLinkQuotaFallsBackWithoutServiceFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Upstream-Host") == "bkliveinfo.ysp.cctv.cn" {
			_, _ = io.WriteString(w, `{"iretcode":0,"playurl":"https://cdn.cctv.cn/live.m3u8"}`)
			return
		}
		_, _ = io.WriteString(w, testPlaylist)
	}))
	defer upstream.Close()
	channels, _ := testManager(t, upstream.URL)
	defer channels.Close()
	manager := newDeviceManager(upstream.Client(), filepath.Join(t.TempDir(), "device.json"))
	for _, slug := range []string{"cctv1", "cctv2", "cctv3", "cctv4", "cctv5", "cctv5p"} {
		manager.linked[slug] = struct{}{}
		manager.lastAccess[slug] = time.Now()
	}
	app := newAppServer(channels, "127.0.0.1", 8767)
	app.device = manager
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/cctv7.m3u8", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("X-YSP-Source") == "device-go" {
		t.Fatalf("quota fallback status=%d source=%q body=%s", recorder.Code, recorder.Header().Get("X-YSP-Source"), recorder.Body.String())
	}
}

func TestDeviceBusinessErrorDoesNotDestroySession(t *testing.T) {
	for _, tc := range []struct {
		status      int
		invalidated bool
	}{
		{status: http.StatusBadRequest, invalidated: false},
		{status: http.StatusUnauthorized, invalidated: true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"result":"rejected"}`, tc.status)
			}))
			defer upstream.Close()
			manager := newDeviceManager(upstream.Client(), filepath.Join(t.TempDir(), "device.json"))
			manager.controlInterval = 0
			manager.endpoints.Live01 = upstream.URL
			session := &deviceSession{Identity: buildDeviceIdentity(testDeviceProfile(), time.Now()), Profile: testDeviceProfile(), CreatedAt: time.Now()}
			manager.session = session
			manager.linked["cctv10"] = struct{}{}
			if _, err := manager.resolveEntry(context.Background(), "cctv10"); err == nil {
				t.Fatal("rejected live request unexpectedly succeeded")
			}
			manager.mu.Lock()
			gotInvalidated := manager.session == nil
			manager.mu.Unlock()
			if gotInvalidated != tc.invalidated {
				t.Fatalf("session invalidated=%t, want %t", gotInvalidated, tc.invalidated)
			}
		})
	}
	if !shouldInvalidateDeviceSession(&deviceHTTPError{Stage: "live/v1/02", Status: http.StatusBadRequest}) {
		t.Fatal("session-bound live/v1/02 HTTP 400 should invalidate the session")
	}
}

func TestCloudRegistrationUsesBusinessCodeFromHTTP400(t *testing.T) {
	var gets, registers atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/get":
			gets.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			writeTestJSON(w, map[string]any{"result": 2, "data": ""})
		case "/register":
			registers.Add(1)
			writeTestJSON(w, map[string]any{"result": 0, "data": map[string]any{"guid": "registered-guid"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	manager := newDeviceManager(upstream.Client(), filepath.Join(t.TempDir(), "device.json"))
	manager.controlInterval = 0
	manager.endpoints.CloudGet = upstream.URL + "/get"
	manager.endpoints.CloudRegister = upstream.URL + "/register"
	profile := testDeviceProfile()
	guid, err := manager.cloudRegistration(context.Background(), buildDeviceIdentity(profile, time.Now()))
	if err != nil || guid != "registered-guid" {
		t.Fatalf("guid=%q err=%v", guid, err)
	}
	if gets.Load() != 1 || registers.Load() != 1 {
		t.Fatalf("cloud calls get=%d register=%d", gets.Load(), registers.Load())
	}
}

func TestDeviceActivePlaylistRenewsEntryOnlyOnProgress(t *testing.T) {
	var sequence atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq := sequence.Add(1)
		value := strconv.Itoa(int(seq))
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:"+value+"\n#EXTINF:4,\nsegment-"+value+".ts\n")
	}))
	defer upstream.Close()
	manager := newDeviceManager(upstream.Client(), filepath.Join(t.TempDir(), "device.json"))
	now := time.Now()
	entry := &deviceEntry{
		ID: "cctv10-active", Slug: "cctv10", FinalURL: upstream.URL,
		ExpiresAt: now.Add(time.Second), MediaLeaseAt: now.Add(time.Minute),
		SessionUntil: now.Add(time.Hour), Headers: make(http.Header),
	}
	manager.entries[entry.Slug] = entry
	manager.mediaEntries[entry.ID] = entry
	if _, err := manager.FetchPlaylist(context.Background(), entry.Slug, "127.0.0.1:8767"); err != nil {
		t.Fatal(err)
	}
	firstExpiry := entry.ExpiresAt
	if time.Until(firstExpiry) < 50*time.Second {
		t.Fatalf("advancing playlist did not renew entry: %v", firstExpiry)
	}
	entry.ExpiresAt = time.Now().Add(time.Second)
	entry.PlaylistAt = time.Time{}
	if _, err := manager.FetchPlaylist(context.Background(), entry.Slug, "127.0.0.1:8767"); err != nil {
		t.Fatal(err)
	}
	if time.Until(entry.ExpiresAt) < 50*time.Second {
		t.Fatal("second advancing playlist did not renew entry")
	}
}

func TestDeviceOldMediaLeaseSurvivesSessionRotation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Playback-Key") != "old" {
			http.Error(w, "wrong playback credentials", http.StatusForbidden)
			return
		}
		_, _ = w.Write(append([]byte{0x47}, make([]byte, 187)...))
	}))
	defer upstream.Close()
	manager := newDeviceManager(upstream.Client(), filepath.Join(t.TempDir(), "device.json"))
	session := &deviceSession{CreatedAt: time.Now()}
	old := &deviceEntry{
		ID: "cctv10-old", Slug: "cctv10", Headers: http.Header{"X-Playback-Key": {"old"}},
		MediaLeaseAt: time.Now().Add(time.Minute),
	}
	manager.session = session
	manager.entries[old.Slug] = old
	manager.mediaEntries[old.ID] = old
	token := manager.signMediaURL(old.ID, upstream.URL+"/old.ts")
	manager.invalidateSession(session)
	manager.mu.Lock()
	manager.entries[old.Slug] = &deviceEntry{ID: "cctv10-new", Slug: old.Slug, Headers: http.Header{"X-Playback-Key": {"new"}}}
	manager.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/_device/media/"+token, nil)
	recorder := httptest.NewRecorder()
	manager.ServeMedia(recorder, req)
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 188 || recorder.Body.Bytes()[0] != 0x47 {
		t.Fatalf("old media lease status=%d bytes=%d", recorder.Code, recorder.Body.Len())
	}
}

func TestDeviceAllChannelsRotateWithinLinkQuota(t *testing.T) {
	const sessionKey = "0123456789abcdef0123456789abcdef"
	const appSecret = "all-channel-secret"
	seen := make(map[string]int)
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live01":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			seen[stringValue(body["id"])]++
			writeTestJSON(w, map[string]any{"data": map[string]any{"videoList": []any{
				map[string]any{"url": "https://example.invalid/live", "rate": "36p", "rateName": "高码率"},
			}}})
		case "/live02":
			encrypted, err := encryptDeviceValue(appSecret, sessionKey)
			if err != nil {
				t.Fatal(err)
			}
			writeTestJSON(w, map[string]any{"data": map[string]any{"appSecret": encrypted}})
		case "/vdn":
			writeTestJSON(w, map[string]any{"succeed": "1", "url": upstream.URL + "/stream.m3u8"})
		case "/stream.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\nsegment.ts\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	manager := newDeviceManager(upstream.Client(), filepath.Join(t.TempDir(), "device.json"))
	manager.controlInterval = 0
	manager.endpoints.Live01 = upstream.URL + "/live01"
	manager.endpoints.Live02 = upstream.URL + "/live02"
	manager.endpoints.VDN = upstream.URL + "/vdn"
	profile := testDeviceProfile()
	session := &deviceSession{
		Identity: buildDeviceIdentity(profile, time.Now()), Profile: profile,
		SessionKey: sessionKey, CloudGUID: "test-guid", CreatedAt: time.Now(),
	}
	manager.session = session
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for slug := range deviceLiveIDs {
		if _, err := manager.FetchPlaylist(ctx, slug, "127.0.0.1:8767"); err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		manager.mu.Lock()
		if len(manager.linked) > deviceMaxLinks {
			manager.mu.Unlock()
			t.Fatalf("%s exceeded link quota: %d", slug, len(manager.linked))
		}
		manager.lastAccess[slug] = time.Now().Add(-deviceLinkIdleTimeout - time.Second)
		manager.mu.Unlock()
	}
	if len(seen) != len(deviceLiveIDs) {
		t.Fatalf("resolved %d unique live IDs, want %d", len(seen), len(deviceLiveIDs))
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.session != session {
		t.Fatal("healthy all-channel rotation replaced the device session")
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
