package config

import (
	"net/textproto"
	"net/url"
	"strings"

	"github.com/example/gitops-dashboard/internal/sanitizer"
)

func appendAlertRedactValues(values []string, extra ...string) []string {
	for _, value := range extra {
		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func appendAlertURLRedactValues(values []string, urls ...string) []string {
	for _, raw := range urls {
		values = appendAlertRedactValues(values, alertURLRedactionValues(raw)...)
	}
	return values
}

func appendAlertHeaderRedactValues(values []string, name, raw string) []string {
	return appendAlertRedactValues(values, alertHeaderRedactionValues(name, raw)...)
}

func AlertingRedactionValues(alerting AlertingConfig) []string {
	values := []string{}
	values = appendAlertURLRedactValues(values, alerting.Sinks.Webhook.URL)
	for name, value := range alerting.Sinks.Webhook.Headers {
		values = appendAlertHeaderRedactValues(values, name, value)
	}
	values = appendAlertDeclaredURLSecretValues(values, alerting.Sinks.Webhook.URL, alerting.Sinks.Webhook.RedactValues)
	values = appendAlertURLRedactValues(values, alerting.Sinks.Discord.WebhookURL)
	// The Discord webhook URL's trailing id/token path segments are the
	// credential by protocol definition (.../api/webhooks/{id}/{token}), and
	// the URL as a whole is a single-purpose credential too.
	values = appendAlertRedactValues(values, alerting.Sinks.Discord.WebhookURL)
	values = appendAlertURLTrailingPathSecretValues(values, alerting.Sinks.Discord.WebhookURL, 2)
	values = appendAlertRedactValues(values, alerting.Sinks.Discord.RedactValues...)
	values = appendAlertURLRedactValues(values, alerting.Sinks.HomeAssistant.BaseURL)
	values = appendAlertRedactValues(values, alerting.Sinks.HomeAssistant.Token, alerting.Sinks.HomeAssistant.WebhookID)
	values = appendAlertRedactValues(values, alerting.Sinks.HomeAssistant.RedactValues...)
	values = appendAlertRedactValues(values, alerting.RedactionValues...)
	return values
}

// alertURLRedactionValues extracts redaction candidates that can be derived
// deterministically from a URL: embedded userinfo credentials and query
// parameters whose name is a recognized secret name (token, key, secret,
// etc; see isAlertSecretParameterName). It intentionally does not attempt to
// guess which path segments are secret-bearing by inspecting their contents
// (e.g. "looks random") — that kind of entropy heuristic is unreliable (a
// purely numeric or purely lowercase secret looks identical to a benign path
// word) and has historically both missed real secrets and risked redacting
// benign words. Path-embedded secrets for the generic webhook sink must be
// declared explicitly; see appendAlertDeclaredURLSecretValues.
func alertURLRedactionValues(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	values := []string{}
	values = appendAlertRedactValues(values, sanitizer.URLUserinfoValues(raw)...)
	values = appendAlertURLQueryRedactValues(values, parsed.RawQuery)
	for key, queryValues := range parsed.Query() {
		if !isAlertSecretParameterName(key) {
			continue
		}
		for _, value := range queryValues {
			values = appendAlertRedactValues(values, alertEscapedSecretVariants(url.QueryEscape(value), value)...)
		}
	}
	return values
}

// appendAlertURLTrailingPathSecretValues registers the last count path
// segments of a URL as redaction candidates unconditionally, regardless of
// their content. This is not content-based guessing: for a fixed-shape
// webhook URL such as Discord's `.../api/webhooks/{id}/{token}`, the
// trailing segments are secret by protocol definition, so every occurrence
// is a "complete secret-bearing path component" the operator never has to
// declare.
func appendAlertURLTrailingPathSecretValues(values []string, rawURL string, count int) []string {
	if count <= 0 {
		return values
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return values
	}
	segments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	start := len(segments) - count
	if start < 0 {
		start = 0
	}
	for _, escaped := range segments[start:] {
		if escaped == "" {
			continue
		}
		if decoded, err := url.PathUnescape(escaped); err == nil {
			values = appendAlertRedactValues(values, alertEscapedSecretVariants(escaped, decoded)...)
		}
	}
	return values
}

// appendAlertDeclaredURLSecretValues registers explicitly declared secret
// values (alerting.sinks.webhook.redactValues) as redaction candidates. Each
// declared value is registered as-is plus its path/query-escaped variants; if
// the value also appears (in decoded form) as a literal path or query
// component of rawURL, the exact raw (still-escaped) substring from the URL
// is registered too, so logs containing either the encoded or decoded form
// are covered. This is the explicit, deterministic replacement for automatic
// path-segment secret detection: the operator states what is secret rather
// than the code guessing from character composition.
func appendAlertDeclaredURLSecretValues(values []string, rawURL string, declared []string) []string {
	declaredSet := map[string]struct{}{}
	for _, value := range declared {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		declaredSet[value] = struct{}{}
		values = appendAlertRedactValues(values, alertEscapedSecretVariants(value, value)...)
	}
	if len(declaredSet) == 0 {
		return values
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return values
	}
	for _, escapedPath := range alertEscapedPathVariants(parsed) {
		for _, part := range strings.Split(escapedPath, "/") {
			if part == "" {
				continue
			}
			unescaped, err := url.PathUnescape(part)
			if err != nil {
				continue
			}
			if _, ok := declaredSet[unescaped]; ok {
				values = appendAlertRedactValues(values, alertEscapedSecretVariants(part, unescaped)...)
			}
		}
	}
	for _, part := range strings.Split(parsed.RawQuery, "&") {
		if part == "" {
			continue
		}
		_, valuePart, _ := strings.Cut(part, "=")
		unescaped, err := url.QueryUnescape(valuePart)
		if err != nil {
			continue
		}
		if _, ok := declaredSet[unescaped]; ok {
			values = appendAlertRedactValues(values, alertEscapedSecretVariants(valuePart, unescaped)...)
		}
	}
	return values
}

func alertEscapedPathVariants(parsed *url.URL) []string {
	variants := []string{}
	for _, value := range []string{parsed.RawPath, parsed.EscapedPath()} {
		if value == "" {
			continue
		}
		seen := false
		for _, existing := range variants {
			if existing == value {
				seen = true
				break
			}
		}
		if !seen {
			variants = append(variants, value)
		}
	}
	return variants
}

func appendAlertURLQueryRedactValues(values []string, rawQuery string) []string {
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		keyPart, valuePart, _ := strings.Cut(part, "=")
		key, err := url.QueryUnescape(keyPart)
		if err != nil || !isAlertSecretParameterName(key) {
			continue
		}
		value, err := url.QueryUnescape(valuePart)
		if err != nil {
			value = valuePart
		}
		values = appendAlertRedactValues(values, alertEscapedSecretVariants(valuePart, value)...)
	}
	return values
}

func alertEscapedSecretVariants(escaped, decoded string) []string {
	variants := []string{decoded}
	if escaped != "" {
		variants = append(variants, escaped)
	}
	if decoded != "" {
		variants = append(variants, url.PathEscape(decoded), url.QueryEscape(decoded))
	}
	return variants
}

func alertHeaderRedactionValues(name, raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	name = textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
	fields := strings.Fields(raw)
	if strings.EqualFold(name, "Authorization") && len(fields) == 2 {
		return appendAlertRedactValues(nil, raw, fields[1])
	}
	if isAlertSecretParameterName(name) {
		return appendAlertRedactValues(nil, raw)
	}
	return nil
}

func isAlertSecretParameterName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "access-token", "api-key", "apikey", "auth", "authorization", "bearer", "client-secret", "key", "password", "secret", "signature", "sig", "token", "webhook-token":
		return true
	default:
		return strings.HasSuffix(normalized, "-token") ||
			strings.HasSuffix(normalized, "-secret") ||
			strings.HasSuffix(normalized, "-key")
	}
}
