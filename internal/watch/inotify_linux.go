//go:build linux

package watch

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
)

// inotify is the event-driven half of a Watcher on Linux.
//
// The fd is put in non-blocking mode and handed to os.NewFile so that reads go
// through the Go runtime poller and honour a deadline. Without that, a blocking
// read on the inotify fd would ignore context cancellation and the supervisor
// could not be stopped while idle -- which is exactly the kind of process you
// end up killing by command-line pattern.
type inotify struct {
	f *os.File
}

const inotifyMask = syscall.IN_CREATE | syscall.IN_MOVED_TO | syscall.IN_CLOSE_WRITE |
	syscall.IN_DELETE | syscall.IN_MOVED_FROM

func newInotify(dirs []string) (*inotify, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("inotify_init1: %w", err)
	}
	for _, d := range dirs {
		if _, err := syscall.InotifyAddWatch(fd, d, inotifyMask); err != nil {
			_ = syscall.Close(fd)
			return nil, fmt.Errorf("inotify_add_watch %s: %w", d, err)
		}
	}
	return &inotify{f: os.NewFile(uintptr(fd), "inotify")}, nil
}

func (i *inotify) Close() error { return i.f.Close() }

// wait blocks until an event arrives, timeout elapses, or ctx is done.
//
// The caller re-checks the filesystem afterwards regardless of what arrived, so
// the event contents are deliberately not decoded: acting on the event rather
// than on the resulting state is how a watcher ends up believing a file exists
// because it saw a create for it.
func (i *inotify) wait(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := i.f.SetReadDeadline(deadline); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		_, err := i.f.Read(buf)
		done <- err
	}()

	select {
	case <-ctx.Done():
		// Unblock the reader by expiring the deadline immediately; it will
		// return os.ErrDeadlineExceeded and exit.
		_ = i.f.SetReadDeadline(time.Now())
		<-done
		return ctx.Err()
	case err := <-done:
		if err == nil || os.IsTimeout(err) {
			return nil
		}
		return err
	}
}
