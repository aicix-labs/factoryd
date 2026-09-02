//go:build !linux

package watch

import (
	"context"
	"errors"
	"time"
)

// inotify does not exist off Linux. newInotify always fails, so New falls back
// to polling and records why -- rather than a build tag quietly producing a
// watcher that claims to be event-driven.
type inotify struct{}

func newInotify([]string) (*inotify, error) {
	return nil, errors.New("inotify is Linux-only")
}

func (i *inotify) Close() error { return nil }

func (i *inotify) wait(context.Context, time.Duration) error {
	return errors.New("inotify is Linux-only")
}
