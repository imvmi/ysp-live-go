package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testAccessToken = "test-token"

func protectedTestServer(t *testing.T) *appServer {
	t.Helper()
	m := newChannelManager(&http.Client{})
	t.Cleanup(m.Close)
	app := newAppServer(m, nil, "127.0.0.1", 8767)
	sum := sha256.Sum256([]byte(testAccessToken))
	if err := app.configureAccessTokenHash(hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if err := app.configurePublicBaseURL("https://tv.example.test"); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestAccessTokenProtectsRoutesButNotHealth(t *testing.T) {
	app := protectedTestServer(t)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/health", http.StatusOK},
		{"/", http.StatusForbidden},
		{"/?token=wrong", http.StatusForbidden},
		{"/?token=" + testAccessToken, http.StatusOK},
		{"/all.m3u?token=" + testAccessToken, http.StatusOK},
	} {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if recorder.Code != tc.want {
			t.Errorf("GET %s: got %d, want %d", tc.path, recorder.Code, tc.want)
		}
	}
}

func TestAccessTokenPropagatesIntoGeneratedURLs(t *testing.T) {
	app := protectedTestServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/all.m3u?token="+testAccessToken, nil)
	app.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, want := range []string{
		"https://tv.example.test/cctv1.m3u8?token=" + testAccessToken,
		"https://tv.example.test/vod/spring-gala/2026.mp4?token=" + testAccessToken,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("playlist does not contain %q", want)
		}
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
}

func TestAccessConfigurationValidation(t *testing.T) {
	m := newChannelManager(&http.Client{})
	defer m.Close()
	app := newAppServer(m, nil, "127.0.0.1", 8767)
	for _, value := range []string{"bad", strings.Repeat("00", sha256.Size-1)} {
		if err := app.configureAccessTokenHash(value); err == nil {
			t.Errorf("accepted token digest %q", value)
		}
	}
	for _, value := range []string{"example.test", "https://user@example.test", "https://example.test/path"} {
		if err := app.configurePublicBaseURL(value); err == nil {
			t.Errorf("accepted public base URL %q", value)
		}
	}
}
