package alarm

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/model"
)

type Service struct {
	mu       sync.RWMutex
	alarms   map[uuid.UUID]model.Alarm
	delivery *DeliveryService
	now      func() time.Time

	snapsMu   sync.RWMutex
	snaps     map[uint64]map[uuid.UUID]model.Alarm
	snapOrder []uint64
	snapSeq   uint64
}

// maxQuerySnapshots bounds how many pinned pagination snapshots are retained.
// Each multi-page walk pins one snapshot; once the bound is exceeded the oldest
// snapshot is evicted and any cursor referencing it is rejected as expired.
const maxQuerySnapshots = 32

func NewService(delivery *DeliveryService, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		alarms:   make(map[uuid.UUID]model.Alarm),
		snaps:    make(map[uint64]map[uuid.UUID]model.Alarm),
		delivery: delivery,
		now:      now,
	}
}

func (s *Service) Raise(ctx context.Context, vesselID string, kind model.AlarmKind, message string) (model.Alarm, error) {
	alarm, err := model.NewAlarm(vesselID, kind, message, s.now())
	if err != nil {
		return model.Alarm{}, err
	}
	s.mu.Lock()
	s.alarms[alarm.ID] = alarm
	s.mu.Unlock()
	delivered, err := s.delivery.Deliver(ctx, alarm)
	s.mu.Lock()
	s.alarms[alarm.ID] = delivered
	s.mu.Unlock()
	return delivered, err
}

func (s *Service) Restore(alarm model.Alarm) {
	s.mu.Lock()
	s.alarms[alarm.ID] = alarm
	s.mu.Unlock()
}

// RecordRetryResult merges the latest per-channel delivery outcome produced by
// a notification retry into the stored alarm. Lifecycle actions taken by an
// operator (acknowledge/clear) are never regressed, but the channel results and
// timestamp are always refreshed so expanded queries show the most recent
// dispatch attempt.
func (s *Service) RecordRetryResult(updated model.Alarm) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.alarms[updated.ID]
	if !ok {
		return
	}
	existing.Channels = append([]model.ChannelResult(nil), updated.Channels...)
	switch existing.State {
	case model.AlarmAcknowledged, model.AlarmCleared:
		// Operator resolution is authoritative; only channel results move.
	default:
		existing.State = updated.State
	}
	if updated.UpdatedAt.After(existing.UpdatedAt) {
		existing.UpdatedAt = updated.UpdatedAt
	}
	s.alarms[updated.ID] = existing
}

func (s *Service) Get(id uuid.UUID) (model.Alarm, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	alarm, ok := s.alarms[id]
	return alarm, ok
}

func (s *Service) List() []model.Alarm {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]model.Alarm, 0, len(s.alarms))
	for _, alarm := range s.alarms {
		result = append(result, alarm)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

func (s *Service) Acknowledge(id uuid.UUID) (model.Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	alarm, ok := s.alarms[id]
	if !ok {
		return model.Alarm{}, errors.New("alarm not found")
	}
	alarm = alarm.Acknowledge(s.now())
	s.alarms[id] = alarm
	return alarm, nil
}

func (s *Service) Clear(id uuid.UUID) (model.Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	alarm, ok := s.alarms[id]
	if !ok {
		return model.Alarm{}, errors.New("alarm not found")
	}
	alarm = alarm.Clear(s.now())
	s.alarms[id] = alarm
	return alarm, nil
}

// snapshot returns a point-in-time copy of every stored alarm. The copy is the
// basis for one cursor walk, so concurrent raises and state changes cannot
// interleave with paging.
func (s *Service) snapshot() map[uuid.UUID]model.Alarm {
	s.mu.RLock()
	defer s.mu.RUnlock()
	captured := make(map[uuid.UUID]model.Alarm, len(s.alarms))
	for id, alarmValue := range s.alarms {
		captured[id] = alarmValue
	}
	return captured
}

// pinSnapshot stores a fresh snapshot and returns its id, evicting the oldest
// retained snapshot when the retention bound is reached.
func (s *Service) pinSnapshot() uint64 {
	s.snapsMu.Lock()
	defer s.snapsMu.Unlock()
	s.snapSeq++
	id := s.snapSeq
	s.snaps[id] = s.snapshot()
	s.snapOrder = append(s.snapOrder, id)
	if len(s.snapOrder) > maxQuerySnapshots {
		evict := s.snapOrder[0]
		s.snapOrder = s.snapOrder[1:]
		delete(s.snaps, evict)
	}
	return id
}

// pinnedSnapshot fetches a retained pagination snapshot. The second result is
// false when the snapshot has been evicted (the cursor is expired).
func (s *Service) pinnedSnapshot(id uint64) (map[uuid.UUID]model.Alarm, bool) {
	s.snapsMu.RLock()
	defer s.snapsMu.RUnlock()
	captured, ok := s.snaps[id]
	return captured, ok
}

// releaseSnapshot drops a completed walk's snapshot ahead of FIFO eviction.
func (s *Service) releaseSnapshot(id uint64) {
	s.snapsMu.Lock()
	defer s.snapsMu.Unlock()
	if _, ok := s.snaps[id]; !ok {
		return
	}
	delete(s.snaps, id)
	for index, queued := range s.snapOrder {
		if queued == id {
			s.snapOrder = append(s.snapOrder[:index], s.snapOrder[index+1:]...)
			break
		}
	}
}
