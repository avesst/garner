package publish

import (
	"context"
	"sync"
	"time"
)

// Scheduler debounces publish requests: a burst of requests within Delay
// yields one publish (section 3). It is the in-process implementation of
// the publish-request interface; with multiple nodes, requests travel
// through the database instead (section 18).
type Scheduler struct {
	Delay   time.Duration
	Publish func(context.Context) error

	mu      sync.Mutex
	pending bool
	wake    chan struct{}
}

// Request asks for a publish; it never blocks.
func (s *Scheduler) Request() {
	s.mu.Lock()
	s.pending = true
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run publishes after requests until ctx is done.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	wake := s.wake
	s.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.Delay):
		}
		s.mu.Lock()
		run := s.pending
		s.pending = false
		s.mu.Unlock()
		if run {
			// Errors are logged by Publish; a later request retries.
			s.Publish(ctx)
		}
	}
}
