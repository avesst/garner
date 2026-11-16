package publish

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A failed publish is retried without a new request, so a package whose
// publish hit a transient error does not wait for unrelated activity.
func TestSchedulerRetriesFailedPublish(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{})
	s := &Scheduler{RetryMin: 10 * time.Millisecond, RetryMax: 40 * time.Millisecond,
		Publish: func(context.Context) error {
			if calls.Add(1) < 3 {
				return errors.New("database is locked")
			}
			close(done)
			return nil
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Request()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("publish ran %d times and was not retried to success", calls.Load())
	}
	time.Sleep(100 * time.Millisecond) // no further runs once it succeeded
	if n := calls.Load(); n != 3 {
		t.Errorf("publish ran %d times, want 3", n)
	}
}
