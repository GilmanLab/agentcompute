package compute

import (
	"context"
	"sync"
)

// gate is a context-aware mutex keyed by sandbox name.
type gate struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	token chan struct{}
	refs  int
}

func newGate() *gate {
	return &gate{locks: make(map[string]*keyedLock)}
}

func (g *gate) Lock(ctx context.Context, key string) (func(), error) {
	g.mu.Lock()
	lock, ok := g.locks[key]
	if !ok {
		lock = &keyedLock{token: make(chan struct{}, 1)}
		lock.token <- struct{}{}
		g.locks[key] = lock
	}
	lock.refs++
	token := lock.token
	g.mu.Unlock()

	select {
	case <-ctx.Done():
		g.release(key)
		return nil, ctx.Err()
	case <-token:
		return func() {
			token <- struct{}{}
			g.release(key)
		}, nil
	}
}

func (g *gate) release(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	lock, ok := g.locks[key]
	if !ok {
		return
	}
	lock.refs--
	if lock.refs == 0 {
		delete(g.locks, key)
	}
}
