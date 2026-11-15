package diagnostics

import (
	"regexp"
	"strings"
)

var secrets = []*regexp.Regexp{
	regexp.MustCompile(`(?is)-----BEGIN[^\n]*PRIVATE KEY-----.*?-----END[^\n]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\b(?:sk-(?:proj-)?|gh[pousr]_)[a-z0-9_-]{16,}`),
}

var credentialField = regexp.MustCompile(`(?i)((?:proxy-)?authorization|(?:set-)?cookie|password|passwd|secret|token|api[_-]?key)["']?\s*[:=]\s*`)
var credentialURL = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`)

func Redact(s string) string {
	for _, r := range secrets {
		s = r.ReplaceAllString(s, "[redacted]")
	}
	s = credentialURL.ReplaceAllString(s, "${1}[redacted]@")
	var out strings.Builder
	end := 0
	for _, match := range credentialField.FindAllStringSubmatchIndex(s, -1) {
		if match[0] < end {
			continue
		}
		start := match[1]
		out.WriteString(s[end:start])
		end = start
		if start < len(s) && (s[start] == '"' || s[start] == '\'') {
			// Consume the whole quoted value, including escaped quotes and separators.
			quote := s[start]
			out.WriteByte(quote)
			end++
			for end < len(s) && s[end] != quote {
				if s[end] == '\\' && end+1 < len(s) {
					end++
				}
				end++
			}
			out.WriteString("[redacted]")
			if end < len(s) {
				out.WriteByte(quote)
				end++
			}
			continue
		}
		key := strings.ToLower(s[match[2]:match[3]])
		header := strings.HasSuffix(key, "authorization") || strings.HasSuffix(key, "cookie")
		for end < len(s) && s[end] != '\r' && s[end] != '\n' {
			// Header credentials include schemes and every cookie in the line.
			if !header && strings.ContainsRune(" \t,;}", rune(s[end])) {
				break
			}
			end++
		}
		out.WriteString("[redacted]")
	}
	out.WriteString(s[end:])
	return out.String()
}
