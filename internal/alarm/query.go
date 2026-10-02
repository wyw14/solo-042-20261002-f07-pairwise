package alarm

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/model"
)

// MaxPageSize bounds how many alarms a single query page may return.
const MaxPageSize = 200

var (
	ErrInvalidCursor       = errors.New("invalid cursor")
	ErrCursorFilterChanged = errors.New("cursor does not match query parameters")
	ErrPageSize            = errors.New("page_size must be between 1 and 200")
	ErrInvalidFilter       = errors.New("invalid filter")
)

// Filter narrows an alarm query. The zero value matches every alarm.
type Filter struct {
	VesselID      string
	Kind          model.AlarmKind
	State         model.AlarmState
	CreatedAfter  time.Time
	CreatedBefore time.Time
}

func (f Filter) matches(a model.Alarm) bool {
	if f.VesselID != "" && a.VesselID != f.VesselID {
		return false
	}
	if f.Kind != "" && a.Kind != f.Kind {
		return false
	}
	if f.State != "" && a.State != f.State {
		return false
	}
	if !f.CreatedAfter.IsZero() && !a.CreatedAt.After(f.CreatedAfter) {
		return false
	}
	if !f.CreatedBefore.IsZero() && !a.CreatedAt.Before(f.CreatedBefore) {
		return false
	}
	return true
}

func (f Filter) Validate() error {
	switch f.Kind {
	case "", model.AlarmLowLevel, model.AlarmPressure, model.AlarmLeak, model.AlarmProbe:
	default:
		return errors.Join(ErrInvalidFilter, errors.New("unknown kind: "+string(f.Kind)))
	}
	switch f.State {
	case "", model.AlarmDetected, model.AlarmDispatching, model.AlarmPartial,
		model.AlarmDelivered, model.AlarmAcknowledged, model.AlarmCleared:
	// exhaustive list of supported states
	default:
		return errors.Join(ErrInvalidFilter, errors.New("unknown state: "+string(f.State)))
	}
	if !f.CreatedAfter.IsZero() && !f.CreatedBefore.IsZero() &&
		!f.CreatedBefore.After(f.CreatedAfter) {
		return errors.Join(ErrInvalidFilter, errors.New("created_before must be after created_after"))
	}
	return nil
}

// cursorPayload is embedded in every opaque page cursor. It carries the
// immutable ordering key of the last returned row together with the exact
// filter it was produced under, so a cursor can never be replayed against a
// different query or forged.
type cursorPayload struct {
	V    string       `json:"v"`
	Time int64        `json:"t"` // unix nanoseconds, CreatedAt of the last item
	ID   uuid.UUID    `json:"id"`
	F    cursorFilter `json:"f"`
}

type cursorFilter struct {
	VesselID string           `json:"vid,omitempty"`
	Kind     model.AlarmKind  `json:"k,omitempty"`
	State    model.AlarmState `json:"s,omitempty"`
	After    int64            `json:"af,omitempty"`
	Before   int64            `json:"be,omitempty"`
}

func newCursorFilter(f Filter) cursorFilter {
	cf := cursorFilter{VesselID: f.VesselID, Kind: f.Kind, State: f.State}
	if !f.CreatedAfter.IsZero() {
		cf.After = f.CreatedAfter.UnixNano()
	}
	if !f.CreatedBefore.IsZero() {
		cf.Before = f.CreatedBefore.UnixNano()
	}
	return cf
}

func (cf cursorFilter) filter() Filter {
	f := Filter{VesselID: cf.VesselID, Kind: cf.Kind, State: cf.State}
	if cf.After != 0 {
		f.CreatedAfter = time.Unix(0, cf.After).UTC()
	}
	if cf.Before != 0 {
		f.CreatedBefore = time.Unix(0, cf.Before).UTC()
	}
	return f
}

// CursorCodec signs and encodes cursors so callers cannot craft or tamper
// with them, and binds a cursor to the server instance's signing key.
type CursorCodec struct {
	key []byte
}

func NewCursorCodec(secret []byte) (*CursorCodec, error) {
	if len(secret) == 0 {
		return nil, errors.New("cursor signing secret is required")
	}
	return &CursorCodec{key: append([]byte(nil), secret...)}, nil
}

func (c *CursorCodec) sign(body []byte) []byte {
	mac := hmac.New(sha256.New, c.key)
	mac.Write(body)
	return mac.Sum(nil)
}

// Encode builds an opaque cursor for the last item on a page.
func (c *CursorCodec) Encode(lastCreatedAt time.Time, lastID uuid.UUID, filter Filter) (string, error) {
	payload := cursorPayload{
		V:    "al1",
		Time: lastCreatedAt.UnixNano(),
		ID:   lastID,
		F:    newCursorFilter(filter),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString(c.sign(body)), nil
}

// Decode validates the signature and shape of a cursor supplied by a client
// and returns its ordering key and the filter bound to the query.
func (c *CursorCodec) Decode(token string) (createdAt time.Time, id uuid.UUID, filter Filter, err error) {
	bodyB64, sigB64, ok := strings.Cut(token, ".")
	if !ok || bodyB64 == "" || sigB64 == "" {
		return time.Time{}, uuid.Nil, Filter{}, ErrInvalidCursor
	}
	body, decodeErr := base64.RawURLEncoding.DecodeString(bodyB64)
	if decodeErr != nil {
		return time.Time{}, uuid.Nil, Filter{}, ErrInvalidCursor
	}
	signature, decodeErr := base64.RawURLEncoding.DecodeString(sigB64)
	if decodeErr != nil || !hmac.Equal(signature, c.sign(body)) {
		return time.Time{}, uuid.Nil, Filter{}, ErrInvalidCursor
	}
	var payload cursorPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return time.Time{}, uuid.Nil, Filter{}, ErrInvalidCursor
	}
	if payload.V != "al1" || payload.ID == uuid.Nil || payload.Time == 0 {
		return time.Time{}, uuid.Nil, Filter{}, ErrInvalidCursor
	}
	return time.Unix(0, payload.Time).UTC(), payload.ID, payload.F.filter(), nil
}

// Page is one stable slice of a filtered alarm query.
type Page struct {
	Items      []model.Alarm
	NextCursor string
	HasMore    bool
}

// Query returns at most pageSize alarms matching filter in ascending
// (CreatedAt, ID) order starting after the supplied cursor.
//
// The snapshot is taken under a single read lock, so concurrent raises and
// state changes cannot interleave with a page fetch: each page is a coherent
// slice and the (CreatedAt, ID) keyset cursor keeps the next fetch exactly
// where this one ended regardless of writes that land in between.
func (s *Service) Query(filter Filter, pageSize int, cursor string) (Page, error) {
	if err := filter.Validate(); err != nil {
		return Page{}, err
	}
	if pageSize < 1 || pageSize > MaxPageSize {
		return Page{}, ErrPageSize
	}
	var afterTime time.Time
	var afterID uuid.UUID
	if cursor != "" {
		cursorTime, cursorID, cursorFilter, err := s.cursors.Decode(cursor)
		if err != nil {
			return Page{}, err
		}
		if !filtersEqual(cursorFilter, filter) {
			return Page{}, ErrCursorFilterChanged
		}
		afterTime = cursorTime
		afterID = cursorID
	}

	s.mu.RLock()
	// s.order is already (CreatedAt, ID) ascending, so walking it yields a
	// stable keyset order with no sort and lets us stop once the page plus
	// the has-more probe row is full.
	items := make([]model.Alarm, 0, pageSize+1)
	for _, id := range s.order {
		a, ok := s.alarms[id]
		if !ok {
			continue
		}
		if !filter.matches(a) {
			continue
		}
		if !afterTime.IsZero() && !afterKey(a.CreatedAt, a.ID, afterTime, afterID) {
			continue
		}
		items = append(items, a)
		if len(items) > pageSize {
			break
		}
	}
	s.mu.RUnlock()

	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	page := Page{Items: items, HasMore: hasMore}
	if hasMore {
		last := items[len(items)-1]
		token, err := s.cursors.Encode(last.CreatedAt, last.ID, filter)
		if err != nil {
			return Page{}, err
		}
		page.NextCursor = token
	}
	return page, nil
}

// afterKey reports whether (t, id) is strictly after the cursor position in
// ascending (CreatedAt, ID) order.
func afterKey(t time.Time, id uuid.UUID, cursorTime time.Time, cursorID uuid.UUID) bool {
	if !t.Equal(cursorTime) {
		return t.After(cursorTime)
	}
	return id != cursorID && id.String() > cursorID.String()
}

func sortAlarms(items []model.Alarm) {
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID.String() < items[j].ID.String()
	})
}

func filtersEqual(a, b Filter) bool {
	return a.VesselID == b.VesselID &&
		a.Kind == b.Kind &&
		a.State == b.State &&
		atomicTimeEqual(a.CreatedAfter, b.CreatedAfter) &&
		atomicTimeEqual(a.CreatedBefore, b.CreatedBefore)
}

func atomicTimeEqual(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() == b.IsZero()
	}
	return a.UnixNano() == b.UnixNano()
}
