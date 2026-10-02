package alarm

import (
	"context"
	"crypto/rand"
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
	order    []uuid.UUID // append-only (CreatedAt, ID) index covering every alarm ever raised
	delivery *DeliveryService
	cursors  *CursorCodec
	now      func() time.Time
}

func NewService(delivery *DeliveryService, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic(err)
	}
	codec, err := NewCursorCodec(secret)
	if err != nil {
		panic(err)
	}
	return &Service{
		alarms:   make(map[uuid.UUID]model.Alarm),
		delivery: delivery,
		cursors:  codec,
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
	s.insertOrderLocked(alarm)
	s.mu.Unlock()
	delivered, err := s.delivery.Deliver(ctx, alarm)
	if delivered.ID != uuid.Nil {
		s.mu.Lock()
		if _, ok := s.alarms[delivered.ID]; ok {
			s.alarms[delivered.ID] = delivered
		}
		s.mu.Unlock()
	}
	return delivered, err
}

// RecordDelivery persists a delivery snapshot (e.g. a scheduled retry's
// per-channel results) and applies it to the in-memory alarm.
func (s *Service) RecordDelivery(updated model.Alarm) error {
	persisted, err := s.delivery.Record(updated)
	if err != nil {
		return err
	}
	s.ApplyDelivery(persisted)
	return nil
}

// ApplyDelivery records the latest channel delivery results for an alarm
// (including scheduled retries) without changing its position in query
// ordering. Operator-driven states (acknowledged/cleared) are sticky: a late
// retry result refreshes the per-channel outcomes but never reopens them.
// Unknown alarms are ignored.
func (s *Service) ApplyDelivery(updated model.Alarm) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.alarms[updated.ID]
	if !ok {
		return
	}
	current.Channels = updated.Channels
	current.UpdatedAt = updated.UpdatedAt
	switch current.State {
	case model.AlarmAcknowledged, model.AlarmCleared:
		// keep operator-driven state
	default:
		current.State = updated.State
	}
	s.alarms[updated.ID] = current
}

func (s *Service) Restore(alarm model.Alarm) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.alarms[alarm.ID]; !ok {
		s.insertOrderLocked(alarm)
	}
	s.alarms[alarm.ID] = alarm
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
	return s.snapshotLocked()
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

// snapshotLocked copies every alarm in (CreatedAt, ID) order. Callers must
// hold at least a read lock.
func (s *Service) snapshotLocked() []model.Alarm {
	result := make([]model.Alarm, 0, len(s.alarms))
	for _, id := range s.order {
		if alarm, ok := s.alarms[id]; ok {
			result = append(result, alarm)
		}
	}
	if len(result) != len(s.alarms) {
		// Defensive: map holds an ID missing from the index.
		indexed := make(map[uuid.UUID]struct{}, len(result))
		for _, alarm := range result {
			indexed[alarm.ID] = struct{}{}
		}
		for _, alarm := range s.alarms {
			if _, ok := indexed[alarm.ID]; !ok {
				result = append(result, alarm)
			}
		}
		sortAlarms(result)
	}
	return result
}

// insertOrderLocked places the alarm's ID into the index keeping
// (CreatedAt, ID) ascending. Callers must hold the write lock.
func (s *Service) insertOrderLocked(alarm model.Alarm) {
	position := sort.Search(len(s.order), func(i int) bool {
		current := s.alarms[s.order[i]]
		if !current.CreatedAt.Equal(alarm.CreatedAt) {
			return current.CreatedAt.After(alarm.CreatedAt)
		}
		return current.ID.String() >= alarm.ID.String()
	})
	s.order = append(s.order, uuid.Nil)
	copy(s.order[position+1:], s.order[position:])
	s.order[position] = alarm.ID
}
