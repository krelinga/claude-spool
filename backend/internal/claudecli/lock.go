package claudecli

import (
	"context"
	"sync"
	"time"
)

// Lock serialises every claude subprocess in the process.
//
// This is the mechanism behind the design's "one writer" rule (§3.2): jobs, the
// keep-alive request, and the interactive re-login all hold it, so nothing
// inside the container can race to rotate the refresh token. It is not a
// performance lock and must never be made finer-grained.
type Lock struct {
	mu sync.Mutex

	state sync.Mutex
	held  bool
	who   string
	since time.Time
	now   func() time.Time
}

func NewLock() *Lock { return &Lock{now: time.Now} }

// Acquire blocks until the lock is free or ctx is done. The returned release
// function is idempotent.
func (l *Lock) Acquire(ctx context.Context, who string) (release func(), err error) {
	// sync.Mutex has no context-aware Lock, so wait on a goroutine instead of
	// blocking a shutdown indefinitely.
	got := make(chan struct{})
	go func() {
		l.mu.Lock()
		close(got)
	}()
	select {
	case <-got:
	case <-ctx.Done():
		// Hand the lock straight back once the goroutine wins it, so an
		// abandoned wait cannot leak it.
		go func() {
			<-got
			l.mu.Unlock()
		}()
		return nil, ctx.Err()
	}

	l.setHolder(who)
	var once sync.Once
	return func() {
		once.Do(func() {
			l.setHolder("")
			l.mu.Unlock()
		})
	}, nil
}

// TryAcquire takes the lock only if it is free right now.
func (l *Lock) TryAcquire(who string) (release func(), ok bool) {
	if !l.mu.TryLock() {
		return nil, false
	}
	l.setHolder(who)
	var once sync.Once
	return func() {
		once.Do(func() {
			l.setHolder("")
			l.mu.Unlock()
		})
	}, true
}

func (l *Lock) setHolder(who string) {
	l.state.Lock()
	defer l.state.Unlock()
	if who == "" {
		l.held, l.who, l.since = false, "", time.Time{}
		return
	}
	l.held, l.who = true, who
	if l.now != nil {
		l.since = l.now()
	} else {
		l.since = time.Now()
	}
}

// Holder reports who holds the lock, for /v1/executor and diagnostics.
func (l *Lock) Holder() (who string, since time.Time, held bool) {
	l.state.Lock()
	defer l.state.Unlock()
	return l.who, l.since, l.held
}
