package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Keep only the latest registration attempt. Never retain raw responses or
// identifiers in diagnostics, including when the upstream sends invalid JSON.
type deviceCloudAttempt struct {
	Stage   string
	At      time.Time
	HTTP    int
	Result  int
	HasGUID bool
	Shape   string
	Error   string
}

type deviceDiagnostics struct {
	Cloud  []deviceCloudAttempt
	Errors map[string]string
}

func (m *deviceManager) Diagnostics() deviceDiagnostics {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := deviceDiagnostics{Cloud: append([]deviceCloudAttempt(nil), m.cloudAttempts...), Errors: make(map[string]string, len(m.failures))}
	for slug, failure := range m.failures {
		result.Errors[slug] = failure.Error
	}
	return result
}

// Structure alone is sufficient to diagnose schema changes. String contents,
// numeric values, IDs, cookies and credentials are never included.
func cloudResponseShape(value any, depth int) string {
	if depth >= 4 {
		return "..."
	}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fields := make([]string, 0, min(len(keys), 12))
		for i, key := range keys {
			if i == 12 {
				fields = append(fields, "...")
				break
			}
			label := key
			if len(label) == 0 || len(label) > 48 {
				label = "<field>"
			} else {
				for _, r := range label {
					if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
						label = "<field>"
						break
					}
				}
			}
			fields = append(fields, label+":"+cloudResponseShape(v[key], depth+1))
		}
		return "{" + strings.Join(fields, ",") + "}"
	case []any:
		if len(v) == 0 {
			return "[]"
		}
		return fmt.Sprintf("array(len=%d,first=%s)", len(v), cloudResponseShape(v[0], depth+1))
	case string:
		if v == "" {
			return "empty-string"
		}
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

func (m *deviceManager) recordCloudAttempt(attempt deviceCloudAttempt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Defensive bound also protects diagnostics if the registration flow changes.
	if len(m.cloudAttempts) == 8 {
		m.cloudAttempts = m.cloudAttempts[1:]
	}
	m.cloudAttempts = append(m.cloudAttempts, attempt)
}

type deviceCloudReply struct {
	status, code int
	guid         string
}

func (r deviceCloudReply) failure(stage, reason string) error {
	return fmt.Errorf("cloud device %s HTTP %d result=%d guid=%t: %s", stage, r.status, r.code, r.guid != "", reason)
}

func (m *deviceManager) cloudRegistration(ctx context.Context, identity deviceIdentity) (string, error) {
	m.mu.Lock()
	m.cloudAttempts = nil
	m.mu.Unlock()
	encrypted, err := encryptDeviceID(identity.XUID)
	if err != nil {
		return "", err
	}
	body := map[string]any{"device_name": deviceName, "device_id": encrypted}
	payload, err := marshalDeviceJSON(body, false)
	if err != nil {
		return "", err
	}
	request := func(stage, endpoint string) (deviceCloudReply, error) {
		attempt := deviceCloudAttempt{Stage: stage, At: time.Now(), Result: -1}
		defer func() { m.recordCloudAttempt(attempt) }()
		data, status, err := m.genericRequest(ctx, http.MethodPost, endpoint, freshDeviceHeaders(identity, "application/json", "application/json", 0, ""), payload)
		attempt.HTTP = status
		reply := deviceCloudReply{status: status, code: -1}
		if err != nil {
			attempt.Error = diagnosticError(err, 200)
			return reply, fmt.Errorf("cloud device %s request: %w", stage, err)
		}
		var value map[string]any
		if json.Unmarshal(data, &value) != nil || value == nil {
			attempt.Shape = "invalid-json-object"
			attempt.Error = "invalid JSON object"
			return reply, reply.failure(stage, "invalid JSON object")
		}
		reply.code, reply.guid = resultCode(value), extractDeviceGUID(value)
		attempt.Result, attempt.HasGUID, attempt.Shape = reply.code, reply.guid != "", clipDiagnostic(cloudResponseShape(value, 0), 1200)
		// HTTP 400 carries valid business states (e.g. result=2). Other HTTP
		// failures must not be accepted even if their bodies contain a GUID.
		if status/100 != 2 && status != http.StatusBadRequest {
			attempt.Error = "unexpected HTTP status"
			return reply, reply.failure(stage, attempt.Error)
		}
		if reply.code < 0 {
			attempt.Error = "missing business result code"
			return reply, reply.failure(stage, attempt.Error)
		}
		return reply, nil
	}
	reply, err := request("get", m.endpoints.CloudGet)
	if err != nil {
		return "", err
	}
	switch reply.code {
	case 0:
		if reply.guid != "" {
			return reply.guid, nil
		}
	case 2, 601:
	default:
		return "", reply.failure("get", "lookup rejected")
	}
	// Reuse the same identity and encrypted request. Do not replace an identity
	// or repeatedly register merely because the final lookup is unsuccessful.
	for attempt := 1; attempt <= 2; attempt++ {
		reply, err = request(fmt.Sprintf("register-%d", attempt), m.endpoints.CloudRegister)
		if err != nil {
			return "", err
		}
		if reply.code == 0 && reply.guid != "" {
			return reply.guid, nil
		}
		if reply.code != 695 {
			break
		}
	}
	if reply.code != 0 && reply.code != 694 && reply.code != 2 {
		return "", reply.failure("register", "registration rejected")
	}
	reply, err = request("get-confirm", m.endpoints.CloudGet)
	if err != nil {
		return "", err
	}
	if reply.code != 0 {
		return "", reply.failure("get-confirm", "registration not confirmed")
	}
	if reply.guid == "" {
		return "", reply.failure("get-confirm", "successful lookup returned no supported GUID field")
	}
	return reply.guid, nil
}
