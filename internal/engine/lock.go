package engine

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// fileLock is an exclusive flock on a file.
type fileLock struct{ f *os.File }

func (l *fileLock) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// tryLock takes the lock without waiting; ok is false if someone else holds it.
func tryLock(p string) (l *fileLock, ok bool, err error) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return &fileLock{f: f}, true, nil
	}
	f.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, false, nil
	}
	return nil, false, err
}

// lockPoll is how often acquireLock retries a held lock.
const lockPoll = 250 * time.Millisecond

// acquireLock waits for the lock until ctx is done. flock(2) itself cannot be
// interrupted by a context, so a non-blocking attempt is retried.
func acquireLock(ctx context.Context, p string) (*fileLock, error) {
	t := time.NewTicker(lockPoll)
	defer t.Stop()
	for {
		l, ok, err := tryLock(p)
		if err != nil {
			return nil, err
		}
		if ok {
			return l, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
