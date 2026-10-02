package alarm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/model"
)

const (
	// DefaultPageLimit is applied when the caller does not request a page size.
	DefaultPageLimit = 50
	// MaxPageLimit is the largest page size a client may request.
	MaxPageLimit = 200

	cursorVersion = 1
)

var (
	// ErrInvalidCursor is returned when a continuation token cannot be decoded
	// or does not describe a valid position.
	ErrInvalidCursor = errors.New("invalid cursor")
	// ErrLimitTooLarge is returned when the requested page size exceeds MaxPageLimit.
	ErrLimitTooLarge = errors.New("page limit must not exceed 200")
	// ErrInvalidTimeRange is returned when created_from is later than created_to.
	ErrInvalidTimeRange = errors.New("created_from must not be later than created_to")
)

// Filter narrows an alarm query. Zero values mean the dimension is unconstrained.
type Filter struct {
	VesselID    string
	Kind        model.AlarmKind
	State       model.AlarmState
	CreatedFrom time.Time
	CreatedTo   time.Time
}

// Query is a filtered, cursor-paginated alarm read. Limit <= 0 selects the
// default page size. Cursor is the opaque token returned by a previous Query.
type Query struct {
	Filter
	Limit  int
	Cursor string
}

// Page is one stable slice of the alarm ordering. NextCursor is empty on the
// final page; otherwise it resumes the query after the last returned item.
type Page struct {
	Items      []model.Alarm
	NextCursor string
}

// cursorToken is the wire form of a page position. It points at the immutable
// ordering key (creation time, alarm id); alarm state changes never move an
// alarm within the ordering, so paging cannot duplicate or skip positions.
// The filter is embedded in the token so a cursor can never be resumed against
// different criteria (which could skip matches that precede the position).
type cursorToken struct {
	Version   int    `json:"v"`
	CreatedAt string `json:"ts"`
	ID        string `json:"id"`
	VesselID  string `json:"vid,omitempty"`
	Kind      string `json:"k,omitempty"`
	State     string `json:"st,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Snap      uint64 `json:"snap,omitempty"`

	time time.Time
}

func encodeCursor(filter Filter, snapID uint64, createdAt time.Time, id uuid.UUID) (string, error) {
	token := cursorToken{
		Version:   cursorVersion,
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
		ID:        id.String(),
		VesselID:  filter.VesselID,
		Kind:      string(filter.Kind),
		State:     string(filter.State),
		Snap:      snapID,
	}
	if !filter.CreatedFrom.IsZero() {
		token.From = filter.CreatedFrom.Format(time.RFC3339Nano)
	}
	if !filter.CreatedTo.IsZero() {
		token.To = filter.CreatedTo.Format(time.RFC3339Nano)
	}
	payload, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func (token cursorToken) bindsSameFilter(filter Filter) bool {
	from, to := "", ""
	if !filter.CreatedFrom.IsZero() {
		from = filter.CreatedFrom.Format(time.RFC3339Nano)
	}
	if !filter.CreatedTo.IsZero() {
		to = filter.CreatedTo.Format(time.RFC3339Nano)
	}
	return token.VesselID == filter.VesselID &&
		token.Kind == string(filter.Kind) &&
		token.State == string(filter.State) &&
		token.From == from &&
		token.To == to
}

func decodeCursor(raw string) (cursorToken, error) {
	if raw == "" || len(raw) > 512 {
		return cursorToken{}, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursorToken{}, ErrInvalidCursor
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var token cursorToken
	if err := decoder.Decode(&token); err != nil {
		return cursorToken{}, ErrInvalidCursor
	}
	if token.Version != cursorVersion || token.CreatedAt == "" || token.ID == "" {
		return cursorToken{}, ErrInvalidCursor
	}
	if token.Snap == 0 {
		return cursorToken{}, ErrInvalidCursor
	}
	id, err := uuid.Parse(token.ID)
	if err != nil || id == uuid.Nil {
		return cursorToken{}, ErrInvalidCursor
	}
	at, err := time.Parse(time.RFC3339Nano, token.CreatedAt)
	if err != nil {
		return cursorToken{}, ErrInvalidCursor
	}
	token.time = at.UTC()
	return token, nil
}

func validAlarmKind(kind model.AlarmKind) bool {
	switch kind {
	case model.AlarmLowLevel, model.AlarmPressure, model.AlarmLeak, model.AlarmProbe:
		return true
	}
	return false
}

func validAlarmState(state model.AlarmState) bool {
	switch state {
	case model.AlarmDetected, model.AlarmDispatching, model.AlarmPartial,
		model.AlarmDelivered, model.AlarmAcknowledged, model.AlarmCleared:
		return true
	}
	return false
}

func (q Query) normalize() (Query, cursorToken, error) {
	if q.Limit < 0 {
		return Query{}, cursorToken{}, errors.New("page limit must be a positive integer")
	}
	if q.Limit == 0 {
		q.Limit = DefaultPageLimit
	}
	if q.Limit > MaxPageLimit {
		return Query{}, cursorToken{}, ErrLimitTooLarge
	}
	if q.Kind != "" && !validAlarmKind(q.Kind) {
		return Query{}, cursorToken{}, errors.New("unknown alarm kind")
	}
	if q.State != "" && !validAlarmState(q.State) {
		return Query{}, cursorToken{}, errors.New("unknown alarm state")
	}
	q.VesselID = strings.TrimSpace(q.VesselID)
	q.CreatedFrom = q.CreatedFrom.UTC()
	q.CreatedTo = q.CreatedTo.UTC()
	if !q.CreatedFrom.IsZero() && !q.CreatedTo.IsZero() && q.CreatedFrom.After(q.CreatedTo) {
		return Query{}, cursorToken{}, ErrInvalidTimeRange
	}
	var after cursorToken
	if q.Cursor != "" {
		token, err := decodeCursor(q.Cursor)
		if err != nil {
			return Query{}, cursorToken{}, err
		}
		if !token.bindsSameFilter(q.Filter) {
			return Query{}, cursorToken{}, ErrInvalidCursor
		}
		after = token
	}
	return q, after, nil
}

func (f Filter) matches(alarm model.Alarm) bool {
	if f.VesselID != "" && alarm.VesselID != f.VesselID {
		return false
	}
	if f.Kind != "" && alarm.Kind != f.Kind {
		return false
	}
	if f.State != "" && alarm.State != f.State {
		return false
	}
	if !f.CreatedFrom.IsZero() && alarm.CreatedAt.Before(f.CreatedFrom) {
		return false
	}
	if !f.CreatedTo.IsZero() && alarm.CreatedAt.After(f.CreatedTo) {
		return false
	}
	return true
}

// Query returns one page of alarms matching the filter, ordered oldest-first by
// (created_at, id). The first page of a walk pins a point-in-time snapshot and
// embeds its id in the cursor; every later page reads that same snapshot, so
// concurrent raises and state transitions cannot interleave with the walk and
// no item can be skipped or seen twice. The cursor also resumes from immutable
// position keys, which those mutations never change.
func (s *Service) Query(input Query) (Page, error) {
	query, after, err := input.normalize()
	if err != nil {
		return Page{}, err
	}

	var source map[uuid.UUID]model.Alarm
	var snapID uint64
	if query.Cursor != "" {
		snapID = after.Snap
		var ok bool
		source, ok = s.pinnedSnapshot(snapID)
		if !ok {
			return Page{}, ErrInvalidCursor
		}
	} else {
		snapID = s.pinSnapshot()
		source, _ = s.pinnedSnapshot(snapID)
	}

	filtered := make([]model.Alarm, 0, len(source))
	for _, alarmValue := range source {
		if !query.Filter.matches(alarmValue) {
			continue
		}
		if query.Cursor != "" && !positionAfter(alarmValue, after) {
			continue
		}
		filtered = append(filtered, alarmValue)
	}
	sort.Slice(filtered, func(i, j int) bool {
		return beforePosition(filtered[i], filtered[j])
	})

	hasMore := len(filtered) > query.Limit
	if hasMore {
		filtered = filtered[:query.Limit]
	} else if query.Cursor != "" {
		// Walk is complete; release its pinned snapshot promptly.
		s.releaseSnapshot(snapID)
	}
	page := Page{Items: filtered}
	if hasMore && len(filtered) > 0 {
		last := filtered[len(filtered)-1]
		cursor, encodeErr := encodeCursor(query.Filter, snapID, last.CreatedAt, last.ID)
		if encodeErr != nil {
			return Page{}, encodeErr
		}
		page.NextCursor = cursor
	}
	return page, nil
}

// beforePosition orders alarms ascending by creation time with the alarm id as
// a total-order tie breaker for equal timestamps.
func beforePosition(a, b model.Alarm) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID.String() < b.ID.String()
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// positionAfter reports whether a sits strictly after the cursor position.
func positionAfter(a model.Alarm, c cursorToken) bool {
	if a.CreatedAt.UTC().Equal(c.time) {
		return a.ID.String() > c.ID
	}
	return a.CreatedAt.UTC().After(c.time)
}
