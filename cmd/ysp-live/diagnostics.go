package main

import (
	"net/url"
	"regexp"
	"strings"
)

var diagnosticURL = regexp.MustCompile(`https?://[^\s"'<>]+`)

func clipDiagnostic(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

func diagnosticError(err error, limit int) string {
	if err == nil {
		return ""
	}
	text := diagnosticURL.ReplaceAllStringFunc(err.Error(), func(raw string) string {
		if u, parseErr := url.Parse(raw); parseErr == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host + "/[redacted]"
		}
		return "[redacted URL]"
	})
	return clipDiagnostic(strings.Join(strings.Fields(text), " "), limit)
}
