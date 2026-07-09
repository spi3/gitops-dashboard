package scanner

import (
	"path"
	"regexp"
	"strings"

	"github.com/example/gitops-dashboard/internal/config"
)

func shouldScanRepoPath(repo config.RepositoryConfig, rel string) bool {
	return newRepoPathFilter(repo).shouldScan(rel)
}

func shouldSkipRepoDir(repo config.RepositoryConfig, rel string) bool {
	return newRepoPathFilter(repo).shouldSkipDir(rel)
}

type repoPathFilter struct {
	include []repoPathPattern
	exclude []repoPathPattern
}

type repoPathPattern struct {
	value   string
	hasGlob bool
	regex   *regexp.Regexp
	invalid bool
}

func newRepoPathFilter(repo config.RepositoryConfig) repoPathFilter {
	return repoPathFilter{
		include: compileRepoPathPatterns(repo.IncludePaths),
		exclude: compileRepoPathPatterns(repo.ExcludePaths),
	}
}

func compileRepoPathPatterns(patterns []string) []repoPathPattern {
	compiled := make([]repoPathPattern, 0, len(patterns))
	for _, pattern := range patterns {
		compiled = append(compiled, compileRepoPathPattern(pattern))
	}
	return compiled
}

func compileRepoPathPattern(pattern string) repoPathPattern {
	compiled := repoPathPattern{value: normalizeRepoPath(pattern)}
	compiled.hasGlob = hasGlob(compiled.value)
	if compiled.hasGlob && strings.Contains(compiled.value, "**") {
		regex, err := regexp.Compile(globRegex(compiled.value))
		if err != nil {
			compiled.invalid = true
			return compiled
		}
		compiled.regex = regex
	}
	return compiled
}

func (filter repoPathFilter) shouldScan(rel string) bool {
	rel = normalizeRepoPath(rel)
	if rel == "." {
		return true
	}
	if filter.matchesAny(filter.exclude, rel) {
		return false
	}
	if len(filter.include) == 0 {
		return true
	}
	return filter.matchesAny(filter.include, rel)
}

func (filter repoPathFilter) shouldSkipDir(rel string) bool {
	rel = normalizeRepoPath(rel)
	return rel != "." && filter.matchesAny(filter.exclude, rel)
}

func (filter repoPathFilter) matchesAny(patterns []repoPathPattern, rel string) bool {
	for _, pattern := range patterns {
		if pattern.matches(rel) {
			return true
		}
	}
	return false
}

func matchesRepoPath(pattern, rel string) bool {
	return compileRepoPathPattern(pattern).matches(rel)
}

func (pattern repoPathPattern) matches(rel string) bool {
	rel = normalizeRepoPath(rel)
	if pattern.invalid || pattern.value == "." || rel == "." {
		return false
	}
	if !pattern.hasGlob {
		return rel == pattern.value || strings.HasPrefix(rel, pattern.value+"/")
	}
	if pattern.globMatch(rel) {
		return true
	}
	for ancestor := path.Dir(rel); ancestor != "." && ancestor != "/"; ancestor = path.Dir(ancestor) {
		if pattern.globMatch(ancestor) {
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
	return compileRepoPathPattern(pattern).globMatch(rel)
}

func (pattern repoPathPattern) globMatch(rel string) bool {
	if pattern.invalid {
		return false
	}
	if !strings.Contains(pattern.value, "**") {
		ok, err := path.Match(pattern.value, rel)
		return err == nil && ok
	}
	return pattern.regex != nil && pattern.regex.MatchString(rel)
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
