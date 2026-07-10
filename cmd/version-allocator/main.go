// version-allocator allocates a SemVer tag from tab-separated tag and commit input.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/example/gitops-dashboard/internal/ci"
)

var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validField(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && !strings.ContainsAny(s, " \t\r\n\v\f")
}

func parseTagLine(line string) (ci.Tag, error) {
	if strings.Count(line, "\t") != 1 {
		return ci.Tag{}, fmt.Errorf("invalid tag input; expected exactly one tab between tag and commit")
	}
	name, commit, _ := strings.Cut(line, "\t")
	if !validField(name) || !commitID.MatchString(commit) {
		return ci.Tag{}, fmt.Errorf("invalid tag input; tag and commit must be non-empty canonical fields")
	}
	return ci.Tag{Name: name, Commit: commit}, nil
}

func main() {
	bump := flag.String("bump", "", "version component to bump: patch, minor, or major")
	source := flag.String("source", "", "source commit SHA")
	workflowDispatchBump := flag.Bool("workflow-dispatch-bump", false, "read workflow YAML and report whether it supports workflow_dispatch bump")
	flag.Parse()
	if *workflowDispatchBump {
		workflow, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal("read workflow: %v", err)
		}
		supported, err := ci.WorkflowSupportsReleaseDispatch(workflow)
		if err != nil {
			fatal("inspect workflow: %v", err)
		}
		if supported {
			fmt.Println("true")
		} else {
			fmt.Println("false")
		}
		return
	}
	if !commitID.MatchString(*source) {
		fatal("--source must be a lowercase 40-character hexadecimal commit ID")
	}
	tags := make([]ci.Tag, 0)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		tag, err := parseTagLine(scanner.Text())
		if err != nil {
			fatal("%v", err)
		}
		tags = append(tags, tag)
	}
	if err := scanner.Err(); err != nil {
		fatal("read tags: %v", err)
	}
	version, err := ci.AllocateVersion(tags, *source, ci.Bump(*bump))
	if err != nil {
		fatal("allocate version: %v", err)
	}
	fmt.Println(version)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "version-allocator: "+format+"\n", args...)
	os.Exit(1)
}
