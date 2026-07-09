package version

import (
	"fmt"
	"strings"

	"github.com/example/gitops-dashboard/internal/core"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func Current() core.BuildInfo {
	return core.BuildInfo{
		Version:   clean(Version, "dev"),
		Commit:    clean(Commit, "unknown"),
		BuildDate: clean(BuildDate, "unknown"),
	}
}

func String() string {
	info := Current()
	return fmt.Sprintf("gitops-dashboard %s (commit %s, built %s)", info.Version, info.Commit, info.BuildDate)
}

func clean(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
