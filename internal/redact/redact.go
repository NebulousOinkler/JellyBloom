// Package redact strips credentials from text before it is logged: URL
// parameters (api_key, ApiKey, token), MediaBrowser Token="…" headers, and
// JSON fields such as "AccessToken" or "Pw".
package redact

import (
	"log/slog"
	"regexp"
)

// secretAssignment matches a credential-named key followed by = or : and its
// value, in URLs, headers, and JSON. The key and separator are kept.
var secretAssignment = regexp.MustCompile(`(?i)((?:api_?key|access_?token|token|pw|password|secret)["']?\s*[=:]\s*["']?)[^&\s"',}\]]+`)

// String redacts credential values in s.
func String(s string) string {
	return secretAssignment.ReplaceAllString(s, "${1}[REDACTED]")
}

// ReplaceAttr is a slog.HandlerOptions.ReplaceAttr that redacts string and
// error attribute values (including the message).
func ReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, String(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, String(err.Error()))
		}
	}
	return a
}
