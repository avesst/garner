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
	// RetryMin and RetryMax bound the backoff after a failed Publish,
	// which doubles from RetryMin up to RetryMax (defaults 30s and 10m).
	RetryMin, RetryMax time.Duration

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
	retryMin, retryMax := s.RetryMin, s.RetryMax
	if retryMin <= 0 {
		retryMin = 30 * time.Second
	}
	if retryMax < retryMin {
		retryMax = max(10*time.Minute, retryMin)
	}
	var backoff time.Duration
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
		if !run {
			continue
		}
		// Publish logs its errors. A failure is retried with backoff, so a
		// transient error does not leave changes unpublished until some
		// unrelated request; requests meanwhile join the retry.
		if err := s.Publish(ctx); err == nil || ctx.Err() != nil {
			backoff = 0
			continue
		}
		backoff = min(max(2*backoff, retryMin), retryMax)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		s.Request()
	}
}
