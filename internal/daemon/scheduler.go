package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// DaemonScheduler wraps the service.Scheduler with daemon lifecycle
// management: start, stop, and per-user polling.
type DaemonScheduler struct {
	sch   *service.Scheduler
	svc   service.Service
	clock clock.Clock
	log   *slog.Logger
	users []domain.UserID

	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewDaemonScheduler constructs the daemon-level scheduler.
// For AA-M1, users is the list of user IDs to poll (typically just the operator).
func NewDaemonScheduler(svc service.Service, clk clock.Clock, log *slog.Logger, users []domain.UserID) *DaemonScheduler {
	return &DaemonScheduler{
		sch:    service.NewScheduler(svc, clk, log),
		svc:    svc,
		clock:  clk,
		log:    log,
		users:  users,
		stopCh: make(chan struct{}),
	}
}

// Start begins the scheduler loop in a background goroutine.
func (ds *DaemonScheduler) Start() {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.running {
		return
	}
	ds.stopCh = make(chan struct{})
	ds.running = true
	ds.wg.Add(1)
	go ds.loop()
}

// Stop shuts down the scheduler cleanly.
func (ds *DaemonScheduler) Stop() {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if !ds.running {
		return
	}
	ds.running = false
	close(ds.stopCh)
	ds.wg.Wait()
}

func (ds *DaemonScheduler) loop() {
	defer ds.wg.Done()
	for {
		select {
		case <-ds.stopCh:
			return
		default:
		}
		ds.tick()
	}
}

func (ds *DaemonScheduler) tick() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-ds.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer cancel()

	now := ds.clock.Now()

	for _, userID := range ds.users {
		if userID == "" {
			continue
		}
		subs, err := ds.svc.ListSubscriptions(ctx, userID, service.SubscriptionFilter{
			Status: []domain.SubscriptionStatus{domain.SubscriptionActive},
		})
		if err != nil {
			ds.log.Error("list subscriptions failed", slog.String("user_id", string(userID)), slog.Any("error", err))
			continue
		}
		for _, sub := range subs {
			if sub.NextPollAt.After(now) {
				continue
			}
			if sub.ExpiresAt != nil && sub.ExpiresAt.Before(now) {
				ds.log.Info("subscription expired", slog.String("subscription_id", string(sub.ID)))
				if err := ds.svc.ExpireSubscription(ctx, userID, sub.ID); err != nil {
					ds.log.Error("expire subscription failed", slog.String("subscription_id", string(sub.ID)), slog.Any("error", err))
				}
				continue
			}
			if err := ds.sch.PollSubscription(ctx, userID, sub); err != nil {
				ds.log.Error("poll subscription failed", slog.String("subscription_id", string(sub.ID)), slog.Any("error", err))
			}
		}
	}

	// Sleep until next tick or stop signal.
	select {
	case <-ds.clock.After(30 * time.Second):
	case <-ds.stopCh:
	}
}
