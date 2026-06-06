package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
)

// Scheduler polls active subscriptions and drives them through
// PlanQuest → CreateQuest. It is constructed by the daemon and lives
// for the lifetime of the process.
type Scheduler struct {
	service    Service
	clock      clock.Clock
	log        *slog.Logger
	mu         sync.Mutex
	running    bool
	stopCh     chan struct{}
	wg         sync.WaitGroup
	maxBackoff time.Duration
}

// NewScheduler constructs a Scheduler. service and clock are required.
func NewScheduler(svc Service, clk clock.Clock, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		service:    svc,
		clock:      clk,
		log:        log,
		stopCh:     make(chan struct{}),
		maxBackoff: 10 * time.Minute,
	}
}

// Start begins the scheduler loop in a background goroutine.
func (sch *Scheduler) Start() {
	sch.mu.Lock()
	defer sch.mu.Unlock()
	if sch.running {
		return
	}
	sch.stopCh = make(chan struct{})
	sch.running = true
	sch.wg.Add(1)
	go sch.loop()
}

// Stop signals the scheduler to shut down and waits for the loop to exit.
func (sch *Scheduler) Stop() {
	sch.mu.Lock()
	defer sch.mu.Unlock()
	if !sch.running {
		return
	}
	sch.running = false
	close(sch.stopCh)
	sch.wg.Wait()
}

func (sch *Scheduler) loop() {
	defer sch.wg.Done()
	for {
		select {
		case <-sch.stopCh:
			return
		default:
		}
		sch.tick()
	}
}

func (sch *Scheduler) tick() {
	// tick is a no-op at the service level; the daemon drives per-user polling.
	// Sleep briefly to avoid busy-looping if tick is called directly.
	select {
	case <-sch.clock.After(1 * time.Second):
	case <-sch.stopCh:
	}
}

// PollSubscription executes one poll cycle for a single subscription.
func (sch *Scheduler) PollSubscription(ctx context.Context, userID domain.UserID, sub Subscription) error {
	now := sch.clock.Now()

	plan, err := sch.service.PlanQuest(ctx, userID, sub.Goal)
	if err != nil {
		if errors.Is(err, ErrAuthExpired) {
			if pauseErr := sch.service.PauseSubscription(ctx, userID, sub.ID); pauseErr != nil {
				return fmt.Errorf("PollSubscription: pause on auth expiry: %w", pauseErr)
			}
			return nil
		}
		// Transient error: reschedule.
		sch.log.Warn("plan quest failed, rescheduling", slog.String("subscription_id", string(sub.ID)), slog.Any("error", err))
		return sch.scheduleNext(ctx, userID, sub, now, true)
	}

	if len(plan.FireSchedule) == 0 {
		return sch.scheduleNext(ctx, userID, sub, now, true)
	}

	// AA-M1: Resy-only, single intent. Create the quest.
	questID, err := sch.service.CreateQuest(ctx, userID, sub.Goal, CreateOpts{})
	if err != nil {
		sch.log.Warn("create quest failed, rescheduling", slog.String("subscription_id", string(sub.ID)), slog.Any("error", err))
		return sch.scheduleNext(ctx, userID, sub, now, true)
	}

	// Subscribe to quest events and wait for terminal status.
	subCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	err = sch.service.SubscribeQuest(subCtx, userID, questID, func(ev domain.Event) {
		// Event stream consumed; we check final status after subscribe returns.
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		sch.log.Warn("quest subscribe error", slog.String("quest_id", string(questID)), slog.Any("error", err))
	}

	quest, err := sch.service.GetQuest(ctx, userID, questID)
	if err != nil {
		sch.log.Warn("get quest failed, rescheduling", slog.String("subscription_id", string(sub.ID)), slog.Any("error", err))
		return sch.scheduleNext(ctx, userID, sub, now, true)
	}

	if quest.Summary.Status == domain.StatusBooked {
		if err := sch.service.FulfillSubscription(ctx, userID, sub.ID, questID); err != nil {
			return fmt.Errorf("PollSubscription: fulfill: %w", err)
		}
		sch.log.Info("subscription fulfilled",
			slog.String("subscription_id", string(sub.ID)),
			slog.String("quest_id", string(questID)),
		)
		return nil
	}

	// Failed / cancelled / expired: reschedule with backoff.
	return sch.scheduleNext(ctx, userID, sub, now, true)
}

func (sch *Scheduler) scheduleNext(ctx context.Context, userID domain.UserID, sub Subscription, now time.Time, backoff bool) error {
	nextInterval := sub.PollInterval
	if backoff {
		// Backoff is based on subscription age as an approximation for
		// consecutive retry count. A future improvement could persist
		// retry_count in the subscriptions table.
		delta := now.Sub(sub.CreatedAt)
		retries := int(delta / sub.PollInterval)
		nextInterval = sub.PollInterval
		for i := 0; i < retries && nextInterval < sch.maxBackoff; i++ {
			nextInterval *= 2
		}
		if nextInterval > sch.maxBackoff {
			nextInterval = sch.maxBackoff
		}
	}
	nextPoll := now.Add(nextInterval)

	if err := sch.service.UpdateSubscriptionNextPoll(ctx, userID, sub.ID, nextPoll); err != nil {
		return fmt.Errorf("scheduleNext: %w", err)
	}
	sch.log.Debug("subscription rescheduled",
		slog.String("subscription_id", string(sub.ID)),
		slog.Time("next_poll_at", nextPoll),
		slog.Duration("interval", nextInterval),
	)
	return nil
}
