package alerter

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/storage"
)

// Sink delivers a single alert event to an external destination. Deliver
// must respect ctx's deadline and treat every call independently: retry
// scheduling, backoff, and dead-lettering are owned entirely by the caller
// (Worker) via the storage-layer dispatch state machine, not by the sink.
type Sink interface {
	Name() string
	Timeout() time.Duration
	// Matches reports whether event passes this sink's configured
	// include/exclude filters. Deliver is never called for a non-matching
	// event; the worker completes the dispatch as a no-op instead.
	Matches(event storage.AlertEvent) bool
	Deliver(ctx context.Context, event storage.AlertEvent) error
}

// sinkFilters implements the include/exclude routing rules shared by every
// sink. Exclude wins over Include. An empty Include list places no
// restriction on that dimension; a non-empty one requires a match.
type sinkFilters struct {
	include config.AlertSinkFilterConfig
	exclude config.AlertSinkFilterConfig
}

func newSinkFilters(include, exclude config.AlertSinkFilterConfig) sinkFilters {
	return sinkFilters{include: include, exclude: exclude}
}

func (f sinkFilters) matches(event storage.AlertEvent) bool {
	if stringInList(f.exclude.Services, event.ServiceID) || stringInList(f.exclude.Targets, event.Target) {
		return false
	}
	if len(f.include.Services) > 0 && !stringInList(f.include.Services, event.ServiceID) {
		return false
	}
	if len(f.include.Targets) > 0 && !stringInList(f.include.Targets, event.Target) {
		return false
	}
	return true
}

func stringInList(list []string, value string) bool {
	if value == "" {
		return false
	}
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// baseSink holds the fields and behavior common to every sink
// implementation, so each sink only needs to implement Deliver.
type baseSink struct {
	name    string
	timeout time.Duration
	filters sinkFilters
	client  *http.Client
}

func (b baseSink) Name() string                          { return b.name }
func (b baseSink) Timeout() time.Duration                { return b.timeout }
func (b baseSink) Matches(event storage.AlertEvent) bool { return b.filters.matches(event) }

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	// Draining lets the transport reuse the connection instead of forcing a
	// fresh TLS handshake for the next delivery to the same sink.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
