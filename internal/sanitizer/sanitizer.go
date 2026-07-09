package sanitizer

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const redacted = "[REDACTED]"

var urlWithUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^/\s@]+@)([^\s'"<>]+)`)

type Redactor struct {
	tokens []string
}

func URLUserinfoValues(raw string) []string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return nil
	}
	password, ok := parsed.User.Password()
	if !ok || password == "" {
		return nil
	}
	return []string{password, parsed.User.String()}
}

func New(tokens ...string) Redactor {
	return Redactor{tokens: normalizeTokens(tokens)}
}

func Redact(value string, tokens ...string) string {
	return New(tokens...).Redact(value)
}

func StripURLUserinfo(value string) string {
	return redactURLUserinfo(value)
}

func (redactor Redactor) Redact(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	value = redactURLUserinfo(value)
	for _, token := range redactor.tokens {
		value = strings.ReplaceAll(value, token, redacted)
	}
	if len(value) > 1000 {
		value = value[:1000]
	}
	return value
}

func (redactor Redactor) Values() []string {
	values := make([]string, len(redactor.tokens))
	copy(values, redactor.tokens)
	return values
}

func normalizeTokens(tokens []string) []string {
	seen := map[string]struct{}{}
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		seen[token] = struct{}{}
		if escaped := url.PathEscape(token); escaped != token {
			seen[escaped] = struct{}{}
		}
		if escaped := url.QueryEscape(token); escaped != token {
			seen[escaped] = struct{}{}
		}
	}
	normalized := make([]string, 0, len(seen))
	for token := range seen {
		normalized = append(normalized, token)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if len(normalized[i]) != len(normalized[j]) {
			return len(normalized[i]) > len(normalized[j])
		}
		return normalized[i] < normalized[j]
	})
	return normalized
}

func redactURLUserinfo(value string) string {
	return urlWithUserinfo.ReplaceAllString(value, "$1$3")
}
