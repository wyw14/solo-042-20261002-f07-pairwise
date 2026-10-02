package model

import (
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

type AlarmKind string

const (
	AlarmLowLevel AlarmKind = "low_level"
	AlarmPressure AlarmKind = "over_pressure"
	AlarmLeak     AlarmKind = "nitrogen_leak"
	AlarmProbe    AlarmKind = "probe_disconnected"
)

type AlarmState string

const (
	AlarmDetected     AlarmState = "detected"
	AlarmDispatching  AlarmState = "dispatching"
	AlarmPartial      AlarmState = "partial"
	AlarmDelivered    AlarmState = "delivered"
	AlarmAcknowledged AlarmState = "acknowledged"
	AlarmCleared      AlarmState = "cleared"
)

type ChannelResult struct {
	Channel   string    `json:"channel"`
	Required  bool      `json:"required"`
	Delivered bool      `json:"delivered"`
	Error     string    `json:"error,omitempty"`
	Attempted time.Time `json:"attempted_at"`
}

type Alarm struct {
	ID              uuid.UUID       `json:"id"`
	VesselID        string          `json:"vessel_id"`
	Kind            AlarmKind       `json:"kind"`
	State           AlarmState      `json:"state"`
	Message         string          `json:"message"`
	Channels        []ChannelResult `json:"channels"`
	SuppressedUntil time.Time       `json:"suppressed_until,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

func NewAlarm(vesselID string, kind AlarmKind, message string, now time.Time) (Alarm, error) {
	if vesselID == "" || message == "" {
		return Alarm{}, errors.New("alarm vessel and message are required")
	}
	switch kind {
	case AlarmLowLevel, AlarmPressure, AlarmLeak, AlarmProbe:
	default:
		return Alarm{}, errors.New("unknown alarm kind")
	}
	return Alarm{
		ID:        uuid.New(),
		VesselID:  vesselID,
		Kind:      kind,
		State:     AlarmDetected,
		Message:   message,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}, nil
}

func (a Alarm) WithChannels(results []ChannelResult, now time.Time) Alarm {
	a.Channels = append([]ChannelResult(nil), results...)
	a.State = AlarmDelivered
	for _, result := range results {
		if result.Required && !result.Delivered {
			a.State = AlarmPartial
			break
		}
	}
	a.UpdatedAt = now.UTC()
	return a
}

func (a Alarm) Acknowledge(now time.Time) Alarm {
	a.State = AlarmAcknowledged
	a.UpdatedAt = now.UTC()
	return a
}

func (a Alarm) Clear(now time.Time) Alarm {
	a.State = AlarmCleared
	a.UpdatedAt = now.UTC()
	return a
}

// LatestDeliveries returns the most recent dispatch result for each
// notification channel. When a channel appears more than once the attempt
// with the latest AttemptedAt wins. Results are ordered by channel name so
// the expansion is stable across pages.
func (a Alarm) LatestDeliveries() []ChannelResult {
	if len(a.Channels) == 0 {
		return []ChannelResult{}
	}
	latest := make(map[string]ChannelResult, len(a.Channels))
	for _, result := range a.Channels {
		current, ok := latest[result.Channel]
		if !ok || result.Attempted.After(current.Attempted) {
			latest[result.Channel] = result
		}
	}
	results := make([]ChannelResult, 0, len(latest))
	for _, result := range latest {
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Channel < results[j].Channel })
	return results
}
