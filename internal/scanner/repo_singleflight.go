package scanner

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/example/gitops-dashboard/internal/config"
)

var (
	repoScanFlights repoOperationGroup
	repoSyncFlights repoOperationGroup
)

type repoOperationGroup struct {
	mu    sync.Mutex
	calls map[string]*repoOperationCall
}

type repoOperationCall struct {
	done  chan struct{}
	value any
	err   error
}

func (group *repoOperationGroup) do(ctx context.Context, key string, fn func() (any, error)) (any, error) {
	group.mu.Lock()
	if group.calls == nil {
		group.calls = map[string]*repoOperationCall{}
	}
	if call, ok := group.calls[key]; ok {
		group.mu.Unlock()
		select {
		case <-call.done:
			return call.value, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &repoOperationCall{done: make(chan struct{})}
	group.calls[key] = call
	group.mu.Unlock()

	call.value, call.err = fn()

	group.mu.Lock()
	delete(group.calls, key)
	close(call.done)
	group.mu.Unlock()
	return call.value, call.err
}

func (group *repoOperationGroup) doDetached(ctx context.Context, key string, fn func() (any, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	group.mu.Lock()
	if group.calls == nil {
		group.calls = map[string]*repoOperationCall{}
	}
	if call, ok := group.calls[key]; ok {
		group.mu.Unlock()
		select {
		case <-call.done:
			return call.value, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &repoOperationCall{done: make(chan struct{})}
	group.calls[key] = call
	group.mu.Unlock()

	go func() {
		call.value, call.err = fn()
		group.mu.Lock()
		delete(group.calls, key)
		close(call.done)
		group.mu.Unlock()
	}()

	select {
	case <-call.done:
		return call.value, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (scanner Scanner) repoOperationKey(repo config.RepositoryConfig) (string, error) {
	repoCacheDir, err := scanner.ensureRepoCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(repoCacheDir, safeName(repo.Name)), nil
}
