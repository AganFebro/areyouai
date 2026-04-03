package purge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/service/a2a"
)

type fakeSweeper struct {
	result a2a.LifecycleSweepResult
	err    error
	calls  int
	limit  int
}

func (f *fakeSweeper) SweepLifecycle(_ context.Context, limit int) (a2a.LifecycleSweepResult, error) {
	f.calls++
	f.limit = limit
	if f.err != nil {
		return a2a.LifecycleSweepResult{}, f.err
	}
	return f.result, nil
}

func TestRunOnceReturnsSweepResult(t *testing.T) {
	t.Parallel()

	sweeper := &fakeSweeper{
		result: a2a.LifecycleSweepResult{
			Scanned:           9,
			ClosedTransitions: 2,
			PurgedTransitions: 4,
		},
	}
	worker := New(sweeper, Config{
		BatchSize:    250,
		PollInterval: 5 * time.Second,
		Now:          func() time.Time { return time.Unix(1712016000, 0).UTC() },
	})
	out, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if sweeper.calls != 1 {
		t.Fatalf("calls=%d want=1", sweeper.calls)
	}
	if sweeper.limit != 250 {
		t.Fatalf("limit=%d want=250", sweeper.limit)
	}
	if out.Scanned != 9 || out.ClosedTransitions != 2 || out.PurgedTransitions != 4 {
		t.Fatalf("result=%+v", out)
	}
}

func TestRunOncePropagatesError(t *testing.T) {
	t.Parallel()

	sweeper := &fakeSweeper{err: errors.New("boom")}
	worker := New(sweeper, Config{})
	if _, err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}
