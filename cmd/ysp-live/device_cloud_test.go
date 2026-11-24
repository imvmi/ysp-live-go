package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type cloudTestResponse struct {
	status int
	body   string
}

func TestCloudRegistrationFinalLookupValidation(t *testing.T) {
	needRegister := cloudTestResponse{400, `{"result":2,"data":""}`}
	registered := cloudTestResponse{200, `{"result":0,"data":{}}`}
	confirmed := cloudTestResponse{200, `{"result":0,"data":{"guid":"confirmed-private-guid"}}`}
	cases := []struct {
		name                string
		gets, registers     []cloudTestResponse
		wantError, wantGUID string
		wantStages          string
	}{
		{"confirmed", []cloudTestResponse{needRegister, confirmed}, []cloudTestResponse{registered}, "", "confirmed-private-guid", "get,register-1,get-confirm"},
		{"still-unregistered", []cloudTestResponse{needRegister, needRegister}, []cloudTestResponse{registered}, "get-confirm HTTP 400 result=2 guid=false: registration not confirmed", "", "get,register-1,get-confirm"},
		{"missing-guid", []cloudTestResponse{needRegister, registered}, []cloudTestResponse{registered}, "get-confirm HTTP 200 result=0 guid=false: successful lookup returned no supported GUID field", "", "get,register-1,get-confirm"},
		{"unsupported-shape", []cloudTestResponse{needRegister, {200, `{"result":0,"data":"private-guid-in-string"}`}}, []cloudTestResponse{registered}, "no supported GUID field", "", "get,register-1,get-confirm"},
		{"business-error-with-guid", []cloudTestResponse{needRegister, {200, `{"result":601,"data":{"guid":"must-not-accept"}}`}}, []cloudTestResponse{registered}, "result=601 guid=true: registration not confirmed", "", "get,register-1,get-confirm"},
		{"register-error-with-guid", []cloudTestResponse{needRegister}, []cloudTestResponse{{200, `{"result":777,"data":{"guid":"must-not-accept"}}`}}, "result=777 guid=true: registration rejected", "", "get,register-1"},
		{"http-error-with-guid", []cloudTestResponse{needRegister, {503, `{"result":0,"data":{"guid":"must-not-accept"}}`}}, []cloudTestResponse{registered}, "get-confirm HTTP 503 result=0 guid=true: unexpected HTTP status", "", "get,register-1,get-confirm"},
		{"invalid-json", []cloudTestResponse{needRegister, {502, `<html>private-token-do-not-log</html>`}}, []cloudTestResponse{registered}, "HTTP 502 result=-1 guid=false: invalid JSON object", "", "get,register-1,get-confirm"},
		{"missing-result", []cloudTestResponse{needRegister, {200, `{"data":{"guid":"must-not-accept"}}`}}, []cloudTestResponse{registered}, "missing business result code", "", "get,register-1,get-confirm"},
		{"already-registered", []cloudTestResponse{confirmed}, nil, "", "confirmed-private-guid", "get"},
		{"register-retry", []cloudTestResponse{needRegister, confirmed}, []cloudTestResponse{{400, `{"result":695}`}, registered}, "", "confirmed-private-guid", "get,register-1,register-2,get-confirm"},
		{"register-retry-bounded", []cloudTestResponse{needRegister}, []cloudTestResponse{{400, `{"result":695}`}, {400, `{"result":695}`}}, "registration rejected", "", "get,register-1,register-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var gets, registers int
			var requestIdentity string
			identityChanged := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, "bad request", 400)
					return
				}
				if requestIdentity == "" {
					requestIdentity = body["device_id"]
				} else if requestIdentity != body["device_id"] {
					identityChanged = true
				}
				var reply cloudTestResponse
				if r.URL.Path == "/get" {
					if gets >= len(tc.gets) {
						http.Error(w, "unexpected get", 500)
						gets++
						return
					}
					reply = tc.gets[gets]
					gets++
				} else {
					if registers >= len(tc.registers) {
						http.Error(w, "unexpected register", 500)
						registers++
						return
					}
					reply = tc.registers[registers]
					registers++
				}
				w.WriteHeader(reply.status)
				io.WriteString(w, reply.body)
			}))
			defer upstream.Close()
			manager := newDeviceManager(upstream.Client(), filepath.Join(t.TempDir(), "device.json"))
			defer manager.Close()
			manager.controlInterval = 0
			manager.endpoints.CloudGet, manager.endpoints.CloudRegister = upstream.URL+"/get", upstream.URL+"/register"
			guid, err := manager.cloudRegistration(context.Background(), buildDeviceIdentity(testDeviceProfile(), time.Now()))
			if tc.wantError == "" {
				if err != nil || guid != tc.wantGUID {
					t.Fatalf("registration failed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || guid != "" {
				t.Fatalf("unexpected result: error=%v guid-present=%t", err, guid != "")
			}
			snapshot := manager.Diagnostics()
			var stages []string
			for _, attempt := range snapshot.Cloud {
				stages = append(stages, attempt.Stage)
			}
			if strings.Join(stages, ",") != tc.wantStages {
				t.Fatalf("stages=%v", stages)
			}
			serialized, _ := json.Marshal(snapshot)
			for _, secret := range []string{"confirmed-private-guid", "private-guid-in-string", "must-not-accept", "private-token-do-not-log"} {
				if strings.Contains(string(serialized), secret) || (err != nil && strings.Contains(err.Error(), secret)) {
					t.Fatal("cloud diagnostics exposed response contents")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if gets != len(tc.gets) || registers != len(tc.registers) {
				t.Fatalf("get=%d register=%d", gets, registers)
			}
			if identityChanged || requestIdentity == "" {
				t.Fatal("registration changed or lost the device identity")
			}
		})
	}
}

func TestCloudDiagnosticsRetainOnlyLatestAttempt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"result":0,"data":{"guid":"private-guid"}}`)
	}))
	defer upstream.Close()
	m := newDeviceManager(upstream.Client(), "")
	defer m.Close()
	m.controlInterval = 0
	m.endpoints.CloudGet = upstream.URL
	identity := buildDeviceIdentity(testDeviceProfile(), time.Now())
	for i := 0; i < 12; i++ {
		if _, err := m.cloudRegistration(context.Background(), identity); err != nil {
			t.Fatal(err)
		}
		snapshot := m.Diagnostics()
		if len(snapshot.Cloud) != 1 {
			t.Fatal("old cloud attempts accumulated")
		}
		snapshot.Cloud[0].Stage = "mutated"
		if m.Diagnostics().Cloud[0].Stage != "get" {
			t.Fatal("diagnostics exposed internal state")
		}
	}
}

func TestCloudResponseShapeRedactsValuesAndBoundsNesting(t *testing.T) {
	shape := cloudResponseShape(map[string]any{"result": float64(2), "data": map[string]any{"guid": "secret-guid", "token": "secret-token", "nested": []any{map[string]any{"value": "secret-value"}}}}, 0)
	for _, want := range []string{"result:number", "guid:string", "token:string"} {
		if !strings.Contains(shape, want) {
			t.Fatalf("missing %q in %s", want, shape)
		}
	}
	if strings.Contains(shape, "secret") {
		t.Fatal("response values leaked")
	}
	value := any("secret")
	for i := 0; i < 100; i++ {
		value = map[string]any{"data": value}
	}
	if len(cloudResponseShape(value, 0)) > 80 {
		t.Fatal("unbounded shape depth")
	}
}

func TestDiagnosticsSeparateSourceFailures(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Upstream-Host") {
		case "bkliveinfo.ysp.cctv.cn":
			io.WriteString(w, `{"iretcode":12,"errinfo":"`+strings.Repeat("版权限制", 100)+`"}`)
		case "jacc.ysp.cctv.cn":
			http.Error(w, "JCE unavailable", 502)
		default:
			http.Error(w, "CDN unavailable", 503)
		}
	}))
	defer upstream.Close()
	m, _ := testManager(t, upstream.URL)
	ch := m.channels["cctv10"]
	if m.refreshOnce(context.Background(), ch) {
		t.Fatal("failed sources became healthy")
	}
	ch.mu.Lock()
	ch.workerRunning = true
	ch.nextRetry = time.Now().Add(time.Minute)
	ch.mu.Unlock()
	device := newDeviceManager(http.DefaultClient, "")
	defer device.Close()
	device.recordFailure("cctv10", errors.New("cloud device get-confirm HTTP 400 result=2 guid=false: registration not confirmed"))
	device.recordCloudAttempt(deviceCloudAttempt{Stage: "get-confirm", At: time.Now(), HTTP: 400, Result: 2, Shape: "{data:empty-string,result:number}"})
	app := newAppServer(m, "localhost", 8767)
	app.device = device
	app.recordDeviceAttempt(errors.New("registration failed")) // Keep this diagnostic request from contacting the real cloud.
	diag := httptest.NewRecorder()
	app.serveDiagnostics(diag)
	text := diag.Body.String()
	for _, want := range []string{"version=", "会话初始化失败", "stage=get-confirm", "http=400 result=2", "iretcode=12", "jacc returned HTTP 502", "device_err=", "device_bootstrap_retry_at="} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in diagnostic output: %s", want, text)
		}
	}
	if strings.Contains(text, "等待首次使用") || !utf8.ValidString(text) {
		t.Fatal("misleading state or invalid UTF-8 in diagnostics")
	}
	response := httptest.NewRecorder()
	app.serveChannel(response, httptest.NewRequest("GET", "/cctv10.m3u8", nil), "cctv10")
	if response.Code != 503 {
		t.Fatalf("status=%d", response.Code)
	}
	for _, want := range []string{"BK: ", "JCE: jacc returned HTTP 502", "设备源: cloud device get-confirm HTTP 400 result=2"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("503 hid source failure %q: %s", want, response.Body.String())
		}
	}
	m.recordSourceStatus(ch, "bk", nil)
	ch.mu.Lock()
	bk, jce := ch.bkStatus, ch.jceStatus
	ch.mu.Unlock()
	if bk != "ok" || !strings.Contains(jce, "502") {
		t.Fatal("successful BK attempt lost independent JCE status")
	}
}

func TestDiagnosticErrorRedactsURLsAndPreservesUTF8(t *testing.T) {
	err := errors.New("Get \"https://user:password@cdn.example/media/sensitive-token?sign=secret\": " + strings.Repeat("版权限制", 100))
	summary := diagnosticError(err, 80)
	if !strings.Contains(summary, "cdn.example") || !strings.Contains(summary, "版权") || !utf8.ValidString(summary) {
		t.Fatal("unusable error summary", summary)
	}
	for _, secret := range []string{"password", "sensitive-token", "sign=secret"} {
		if strings.Contains(summary, secret) {
			t.Fatal("URL credential leaked")
		}
	}
}
