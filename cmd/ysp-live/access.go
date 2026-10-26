package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const maxAccessTokenLength = 256

func (s *appServer) configureAccessTokenHash(encoded string) error {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		s.accessTokenHash = nil
		return nil
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("YSP_ACCESS_TOKEN_SHA256 must be a 64-character SHA-256 hex digest")
	}
	s.accessTokenHash = append(s.accessTokenHash[:0], decoded...)
	return nil
}

func (s *appServer) configurePublicBaseURL(raw string) error {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		s.publicBaseURL = ""
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("YSP_PUBLIC_BASE_URL must be an absolute HTTP(S) origin without a path, query, or fragment")
	}
	s.publicBaseURL = raw
	return nil
}

func (s *appServer) authorized(r *http.Request) bool {
	if len(s.accessTokenHash) == 0 {
		return true
	}
	token := r.URL.Query().Get("token")
	if token == "" || len(token) > maxAccessTokenLength {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(sum[:], s.accessTokenHash) == 1
}

func (s *appServer) tokenQuery(r *http.Request) string {
	if len(s.accessTokenHash) == 0 {
		return ""
	}
	return "?" + url.Values{"token": {r.URL.Query().Get("token")}}.Encode()
}

func (s *appServer) requestBaseURL(r *http.Request) string {
	if s.publicBaseURL != "" {
		return s.publicBaseURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	if host := validRequestHost(r.Host); host != "" {
		return scheme + "://" + host
	}
	host := s.bind
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, fmt.Sprint(s.port)))
}

func validRequestHost(raw string) string {
	u, err := url.Parse("//" + raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return u.Host
}

func (s *appServer) publicURL(r *http.Request, path string) string {
	u, _ := url.Parse(s.requestBaseURL(r))
	u.Path = path
	u.RawQuery = ""
	if len(s.accessTokenHash) > 0 {
		u.RawQuery = url.Values{"token": {r.URL.Query().Get("token")}}.Encode()
	}
	return u.String()
}
