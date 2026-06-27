package environment

import (
	"path/filepath"
	"strings"
)

var aliases = map[string]string{
	"prod":        "production",
	"production":  "production",
	"stage":       "staging",
	"staging":     "staging",
	"dev":         "development",
	"development": "development",
	"test":        "testing",
	"testing":     "testing",
	"homelab":     "homelab",
	"lab":         "homelab",
	"local":       "local",
}

func Infer(path string) string {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if env, ok := aliases[strings.ToLower(part)]; ok {
			return env
		}
	}
	return ""
}
