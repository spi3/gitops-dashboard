package scanner

import (
	"path"
	"regexp"
	"strings"

	"github.com/example/gitops-dashboard/internal/config"
)

func shouldScanRepoPath(repo config.RepositoryConfig, rel string) bool {
	rel = normalizeRepoPath(rel)
	if rel == "." {
		return true
	}
	if matchesAnyRepoPath(repo.ExcludePaths, rel) {
		return false
	}
	if len(repo.IncludePaths) == 0 {
		return true
	}
	return matchesAnyRepoPath(repo.IncludePaths, rel)
}

func shouldSkipRepoDir(repo config.RepositoryConfig, rel string) bool {
	rel = normalizeRepoPath(rel)
	return rel != "." && matchesAnyRepoPath(repo.ExcludePaths, rel)
}

func matchesAnyRepoPath(patterns []string, rel string) bool {
	for _, pattern := range patterns {
		if matchesRepoPath(pattern, rel) {
			return true
		}
	}
	return false
}

func matchesRepoPath(pattern, rel string) bool {
	pattern = normalizeRepoPath(pattern)
	rel = normalizeRepoPath(rel)
	if pattern == "." || rel == "." {
		return false
	}
	if !hasGlob(pattern) {
		return rel == pattern || strings.HasPrefix(rel, pattern+"/")
	}
	if repoGlobMatch(pattern, rel) {
		return true
	}
	for ancestor := path.Dir(rel); ancestor != "." && ancestor != "/"; ancestor = path.Dir(ancestor) {
		if repoGlobMatch(pattern, ancestor) {
			return true
		}
	}
	return false
}

func normalizeRepoPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = strings.TrimPrefix(value, "./")
	value = strings.Trim(value, "/")
	if value == "" || value == "." {
		return "."
	}
	return path.Clean(value)
}

func hasGlob(value string) bool {
	return strings.ContainsAny(value, "*?[")
}

func repoGlobMatch(pattern, rel string) bool {
	if !strings.Contains(pattern, "**") {
		ok, err := path.Match(pattern, rel)
		return err == nil && ok
	}
	ok, err := regexp.MatchString(globRegex(pattern), rel)
	return err == nil && ok
}

func globRegex(pattern string) string {
	var out strings.Builder
	out.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					out.WriteString("(?:.*/)?")
				} else {
					out.WriteString(".*")
				}
			} else {
				out.WriteString("[^/]*")
			}
		case '?':
			out.WriteString("[^/]")
		default:
			out.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	out.WriteString("$")
	return out.String()
}
