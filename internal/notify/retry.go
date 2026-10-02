package notify

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/model"
)

type RetryItem struct {
	Alarm       model.Alarm
	NextAttempt time.Time
	Attempts    int
}

type RetryQueue struct {
	mu       sync.Mutex
	items    map[uuid.UUID]RetryItem
	interval time.Duration
	now      func() time.Time
	onResult func(model.Alarm)
}

func NewRetryQueue(interval time.Duration, now func() time.Time) *RetryQueue {
	if now == nil {
		now = time.Now
	}
	return &RetryQueue{items: make(map[uuid.UUID]RetryItem), interval: interval, now: now}
}

// SetResultSink registers a callback invoked with the alarm after every retry
// attempt, carrying the latest per-channel delivery results.
func (q *RetryQueue) SetResultSink(sink func(model.Alarm)) {
	q.mu.Lock()
	q.onResult = sink
	q.mu.Unlock()
}

func (q *RetryQueue) Schedule(alarm model.Alarm) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items[alarm.ID] = RetryItem{Alarm: alarm, NextAttempt: q.now().Add(q.interval).UTC()}
}

func (q *RetryQueue) RunDue(ctx context.Context, dispatcher *Dispatcher) []error {
	q.mu.Lock()
	due := make([]RetryItem, 0)
	for _, item := range q.items {
		if !q.now().Before(item.NextAttempt) {
			due = append(due, item)
		}
	}
	q.mu.Unlock()
	errorsFound := make([]error, 0)
	for _, item := range due {
		results, err := dispatcher.Retry(ctx, item.Alarm, item.Alarm.Channels)
		item.Alarm = item.Alarm.WithChannels(results, q.now())
		q.mu.Lock()
		if err == nil {
			delete(q.items, item.Alarm.ID)
		} else {
			item.Attempts++
			item.NextAttempt = q.now().Add(q.interval).UTC()
			q.items[item.Alarm.ID] = item
			errorsFound = append(errorsFound, err)
		}
		sink := q.onResult
		q.mu.Unlock()
		if sink != nil {
			sink(item.Alarm)
		}
	}
	return errorsFound
}

func (q *RetryQueue) Pending() []RetryItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	result := make([]RetryItem, 0, len(q.items))
	for _, item := range q.items {
		result = append(result, item)
	}
	return result
}
