package purge

import (
	"context"
	"log"
	"time"

	"github.com/febrian/areyouai/internal/service/a2a"
)

type Sweeper interface {
	SweepLifecycle(ctx context.Context, limit int) (a2a.LifecycleSweepResult, error)
}

type Config struct {
	PollInterval time.Duration
	BatchSize    int
	Now          func() time.Time
}

type Worker struct {
	sweeper      Sweeper
	pollInterval time.Duration
	batchSize    int
	now          func() time.Time
}

func New(sweeper Sweeper, cfg Config) *Worker {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Second
	}
	if cfg.BatchSize <= 0 || cfg.BatchSize > 5000 {
		cfg.BatchSize = 200
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{
		sweeper:      sweeper,
		pollInterval: cfg.PollInterval,
		batchSize:    cfg.BatchSize,
		now:          cfg.Now,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		if _, err := w.RunOnce(ctx); err != nil {
			log.Printf("purge_worker_run_once_failed err=%v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *Worker) RunOnce(ctx context.Context) (a2a.LifecycleSweepResult, error) {
	start := w.now()
	result, err := w.sweeper.SweepLifecycle(ctx, w.batchSize)
	end := w.now()
	if end.Before(start) {
		end = start
	}
	duration := end.Sub(start)
	if err != nil {
		return a2a.LifecycleSweepResult{}, err
	}
	log.Printf(
		"purge_sweep_completed scanned=%d transitioned_closed=%d transitioned_purged=%d viewer_blocked=%d ready_for_purge=%d duration_ms=%d",
		result.Scanned,
		result.ClosedTransitions,
		result.PurgedTransitions,
		result.ViewerBlocked,
		result.ReadyForPurge,
		duration.Milliseconds(),
	)
	return result, nil
}
