package claudecli

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The invariant the whole design rests on: never two claude processes at once
// (§3.2). A second holder of the refresh token is the failure this prevents.
func TestLockIsExclusive(t *testing.T) {
	l := NewLock()
	var concurrent, maxConcurrent atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Acquire(context.Background(), "worker")
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			n := concurrent.Add(1)
			for {
				m := maxConcurrent.Load()
				if n <= m || maxConcurrent.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			concurrent.Add(-1)
		}()
	}
	wg.Wait()
	if got := maxConcurrent.Load(); got != 1 {
		t.Errorf("observed %d concurrent holders, want 1", got)
	}
}

func TestLockReleaseIsIdempotent(t *testing.T) {
	l := NewLock()
	release, err := l.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	release() // must not panic or free someone else's lock

	release2, err := l.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if who, _, held := l.Holder(); !held || who != "b" {
		t.Errorf("Holder = %q, %v", who, held)
	}
}

// A shutdown must not be stuck behind a long-running job.
func TestAcquireRespectsContext(t *testing.T) {
	l := NewLock()
	release, err := l.Acquire(context.Background(), "holder")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, "waiter"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Acquire = %v, want DeadlineExceeded", err)
	}

	// An abandoned waiter must not have leaked the lock.
	release()
	got, err := l.Acquire(context.Background(), "next")
	if err != nil {
		t.Fatalf("lock leaked after an abandoned wait: %v", err)
	}
	got()
}

func TestTryAcquire(t *testing.T) {
	l := NewLock()
	release, ok := l.TryAcquire("first")
	if !ok {
		t.Fatal("TryAcquire on a free lock failed")
	}
	if _, ok := l.TryAcquire("second"); ok {
		t.Error("TryAcquire succeeded while held")
	}
	release()
	release2, ok := l.TryAcquire("third")
	if !ok {
		t.Error("TryAcquire failed after release")
	}
	release2()
}

func TestHolderReporting(t *testing.T) {
	l := NewLock()
	if _, _, held := l.Holder(); held {
		t.Error("a fresh lock reports itself held")
	}
	release, _ := l.Acquire(context.Background(), "job 01A")
	who, since, held := l.Holder()
	if !held || who != "job 01A" || since.IsZero() {
		t.Errorf("Holder = %q, %v, %v", who, since, held)
	}
	release()
	if _, _, held := l.Holder(); held {
		t.Error("still reported held after release")
	}
}
