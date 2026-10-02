package alarm

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/model"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	codec, err := NewCursorCodec([]byte("test-cursor-secret"))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	return &Service{
		alarms:  make(map[uuid.UUID]model.Alarm),
		cursors: codec,
		now:     func() time.Time { return base },
	}
}

func seedAlarm(t *testing.T, s *Service, vesselID string, kind model.AlarmKind, state model.AlarmState, createdAt time.Time) model.Alarm {
	t.Helper()
	alarm := model.Alarm{
		ID:        uuid.Must(uuid.NewV7()),
		VesselID:  vesselID,
		Kind:      kind,
		State:     state,
		Message:   "seed",
		CreatedAt: createdAt.UTC(),
		UpdatedAt: createdAt.UTC(),
	}
	s.Restore(alarm)
	return alarm
}

func TestQueryPaginatesWithoutGapsOrDuplicates(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	expected := make([]uuid.UUID, 0, 12)
	for i := range 12 {
		alarm := seedAlarm(t, s,
			[]string{"V-03", "V-07"}[i%2],
			[]model.AlarmKind{model.AlarmLowLevel, model.AlarmPressure}[i%2],
			[]model.AlarmState{model.AlarmDetected, model.AlarmAcknowledged}[i%2],
			base.Add(time.Duration(i)*time.Minute))
		expected = append(expected, alarm.ID)
	}
	// Unrelated vessel must never show up under the filter.
	seedAlarm(t, s, "V-99", model.AlarmLeak, model.AlarmCleared, base.Add(30*time.Minute))

	seen := make(map[uuid.UUID]int)
	cursor := ""
	pageCount := 0
	for {
		page, err := s.Query(Filter{VesselID: "V-03"}, 3, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 3 {
			t.Fatalf("page too large: %d", len(page.Items))
		}
		for _, alarm := range page.Items {
			if alarm.VesselID != "V-03" {
				t.Fatalf("filter leaked vessel %q", alarm.VesselID)
			}
			seen[alarm.ID]++
		}
		pageCount++
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Fatal("next_cursor must be empty on the final page")
			}
			break
		}
		if page.NextCursor == "" {
			t.Fatal("next_cursor required when has_more is true")
		}
		cursor = page.NextCursor
	}
	if pageCount != 2 {
		t.Fatalf("pageCount=%d, want 2", pageCount)
	}
	for _, id := range expected {
		alarm, ok := s.Get(id)
		if !ok {
			t.Fatal("seed alarm missing")
		}
		if alarm.VesselID != "V-03" {
			continue
		}
		if seen[id] != 1 {
			t.Fatalf("alarm %s seen %d times, want exactly 1", id, seen[id])
		}
	}
}

func TestQueryCombinedFilters(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	t1 := seedAlarm(t, s, "V-03", model.AlarmPressure, model.AlarmPartial, base)
	seedAlarm(t, s, "V-03", model.AlarmPressure, model.AlarmAcknowledged, base.Add(time.Minute))
	seedAlarm(t, s, "V-03", model.AlarmLowLevel, model.AlarmPartial, base.Add(2*time.Minute))
	seedAlarm(t, s, "V-07", model.AlarmPressure, model.AlarmPartial, base.Add(3*time.Minute))

	page, err := s.Query(Filter{
		VesselID:      "V-03",
		Kind:          model.AlarmPressure,
		State:         model.AlarmPartial,
		CreatedAfter:  t1.CreatedAt.Add(-time.Second),
		CreatedBefore: t1.CreatedAt.Add(30 * time.Second),
	}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != t1.ID {
		t.Fatalf("combined filter returned %v", page.Items)
	}

	// Bounds are exclusive: equal timestamps fall outside the window.
	page, err = s.Query(Filter{CreatedAfter: t1.CreatedAt}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, alarm := range page.Items {
		if !alarm.CreatedAt.After(t1.CreatedAt) {
			t.Fatal("created_after must be exclusive")
		}
	}
}

func TestQueryRejectsBadCursors(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	a := seedAlarm(t, s, "V-03", model.AlarmLowLevel, model.AlarmDetected, base)
	seedAlarm(t, s, "V-03", model.AlarmLowLevel, model.AlarmDetected, base.Add(time.Minute))
	page, err := s.Query(Filter{}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	validCursor := page.NextCursor

	for _, token := range []string{
		"garbage",
		"abc.def",          // malformed base64
		"eyJ2Ijoie30ifQ.x", // decodable body, bad signature
	} {
		if _, err := s.Query(Filter{}, 1, token); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("token %q: err=%v, want ErrInvalidCursor", token, err)
		}
	}

	// A cursor minted with a different signing key is invalid.
	otherCodec, err := NewCursorCodec([]byte("a-different-secret"))
	if err != nil {
		t.Fatal(err)
	}
	foreignCursor, err := otherCodec.Encode(a.CreatedAt, a.ID, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(Filter{}, 1, foreignCursor); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("foreign cursor err=%v, want ErrInvalidCursor", err)
	}

	// Tampering with the body invalidates the signature.
	tampered := "AAAA" + validCursor[4:]
	if _, err := s.Query(Filter{}, 1, tampered); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("tampered cursor err=%v, want ErrInvalidCursor", err)
	}

	// A valid cursor cannot be replayed with different query parameters.
	if _, err := s.Query(Filter{VesselID: "V-07"}, 1, validCursor); !errors.Is(err, ErrCursorFilterChanged) {
		t.Fatalf("mismatched cursor err=%v, want ErrCursorFilterChanged", err)
	}
}

func TestQueryRejectsOutOfRangePageSize(t *testing.T) {
	s := newTestService(t)
	for _, size := range []int{0, -1, MaxPageSize + 1, 1000000} {
		if _, err := s.Query(Filter{}, size, ""); !errors.Is(err, ErrPageSize) {
			t.Fatalf("size=%d err=%v, want ErrPageSize", size, err)
		}
	}
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	seedAlarm(t, s, "V-03", model.AlarmLowLevel, model.AlarmDetected, base)
	if _, err := s.Query(Filter{}, MaxPageSize, ""); err != nil {
		t.Fatalf("max page size rejected: %v", err)
	}
}

func TestQueryRejectsInvalidFilters(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	cases := []Filter{
		{Kind: "bogus"},
		{State: "bogus"},
		{CreatedAfter: base, CreatedBefore: base},
		{CreatedAfter: base.Add(time.Minute), CreatedBefore: base},
	}
	for index, filter := range cases {
		if _, err := s.Query(filter, 10, ""); !errors.Is(err, ErrInvalidFilter) {
			t.Fatalf("case %d: err=%v, want ErrInvalidFilter", index, err)
		}
	}
}

func TestQueryStaysStableWhileWritesAndStatesChange(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	original := make([]model.Alarm, 0, 10)
	for i := range 10 {
		original = append(original, seedAlarm(t, s, "V-03", model.AlarmLowLevel, model.AlarmDetected,
			base.Add(time.Duration(i)*time.Minute)))
	}

	first, err := s.Query(Filter{VesselID: "V-03"}, 4, "")
	if err != nil {
		t.Fatal(err)
	}
	firstPageIDs := make(map[uuid.UUID]struct{}, len(first.Items))
	for _, alarm := range first.Items {
		firstPageIDs[alarm.ID] = struct{}{}
	}

	// Between page 1 and page 2: a new alarm lands and an upcoming alarm
	// changes state (acknowledged). The keyset cursor must neither resend
	// page 1 nor skip the state-changed row.
	newAlarm := seedAlarm(t, s, "V-03", model.AlarmLowLevel, model.AlarmDetected,
		base.Add(20*time.Minute))
	upcoming := original[6]
	acknowledged := upcoming.Acknowledge(upcoming.CreatedAt.Add(2 * time.Minute))
	s.Restore(acknowledged)

	seen := make(map[uuid.UUID]int)
	for _, alarm := range first.Items {
		seen[alarm.ID]++
	}
	cursor := first.NextCursor
	pages := 1
	for {
		page, err := s.Query(Filter{VesselID: "V-03"}, 4, cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, alarm := range page.Items {
			seen[alarm.ID]++
			if _, repeated := firstPageIDs[alarm.ID]; repeated {
				t.Fatalf("alarm %s from page 1 appeared again", alarm.ID)
			}
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}

	// Every originally matching row (state changes do not affect a vessel
	// filter) appears exactly once, including the acknowledged one.
	for _, alarm := range original {
		if seen[alarm.ID] != 1 {
			t.Fatalf("original alarm %s seen %d times", alarm.ID, seen[alarm.ID])
		}
	}
	if got := acknowledged; seen[got.ID] != 1 {
		t.Fatalf("state-changed alarm seen %d times", seen[got.ID])
	}
	// The concurrently written alarm is reached exactly once as well.
	if seen[newAlarm.ID] != 1 {
		t.Fatalf("newly written alarm seen %d times", seen[newAlarm.ID])
	}
	if pages < 2 {
		t.Fatalf("expected multiple pages, got %d", pages)
	}
}

func TestQueryHandlesEqualTimestampsByID(t *testing.T) {
	s := newTestService(t)
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	ids := make(map[uuid.UUID]struct{}, 5)
	for range 5 {
		alarm := seedAlarm(t, s, "V-03", model.AlarmLowLevel, model.AlarmDetected, at)
		ids[alarm.ID] = struct{}{}
	}
	seen := make(map[uuid.UUID]int)
	cursor := ""
	for {
		page, err := s.Query(Filter{VesselID: "V-03"}, 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		var previous uuid.UUID
		for _, alarm := range page.Items {
			seen[alarm.ID]++
			if previous != uuid.Nil && alarm.ID.String() <= previous.String() {
				t.Fatal("same-timestamp rows not ordered by ID")
			}
			previous = alarm.ID
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("collected %d of 5 equal-timestamp alarms", len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("alarm %s seen %d times", id, count)
		}
	}
}

func TestApplyDeliveryKeepsOperatorStateSticky(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	alarm := seedAlarm(t, s, "V-03", model.AlarmLowLevel, model.AlarmDetected, base)

	acknowledged, err := s.Acknowledge(alarm.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A late retry reports partial delivery with fresh channel results.
	retry := acknowledged.WithChannels([]model.ChannelResult{
		{Channel: "siren", Required: true, Delivered: false, Attempted: base.Add(time.Minute)},
	}, base.Add(time.Minute))
	s.ApplyDelivery(retry)

	stored, ok := s.Get(alarm.ID)
	if !ok {
		t.Fatal("alarm missing")
	}
	if stored.State != model.AlarmAcknowledged {
		t.Fatalf("state=%s, want acknowledged to remain sticky", stored.State)
	}
	if len(stored.Channels) != 1 || stored.Channels[0].Channel != "siren" {
		t.Fatalf("channels not refreshed: %v", stored.Channels)
	}

	// Unknown alarms are ignored rather than inserted.
	s.ApplyDelivery(model.Alarm{ID: uuid.New(), VesselID: "V-03", State: model.AlarmPartial})
	if _, ok := s.Get(uuid.New()); ok {
		t.Fatal("ApplyDelivery must not create alarms")
	}
}

func TestCursorRoundTripRejectsMalformedPayloads(t *testing.T) {
	codec, err := NewCursorCodec([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	id := uuid.Must(uuid.NewV7())
	filter := Filter{VesselID: "V-03", Kind: model.AlarmLeak, State: model.AlarmPartial}
	token, err := codec.Encode(at, id, filter)
	if err != nil {
		t.Fatal(err)
	}
	gotTime, gotID, gotFilter, err := codec.Decode(token)
	if err != nil {
		t.Fatal(err)
	}
	if !gotTime.Equal(at) || gotID != id || !filtersEqual(gotFilter, filter) {
		t.Fatalf("round trip mismatch: %v %v %v", gotTime, gotID, gotFilter)
	}

	for _, raw := range []string{"", "nodot", ".", "a.b", "eyJhIjoxfQ." + token} {
		if _, _, _, err := codec.Decode(raw); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("raw=%q err=%v", raw, err)
		}
	}
}

func TestLatestDeliveriesPicksNewestPerChannel(t *testing.T) {
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	alarm := model.Alarm{Channels: []model.ChannelResult{
		{Channel: "siren", Required: true, Delivered: false, Error: "boom", Attempted: base},
		{Channel: "duty-terminal", Required: true, Delivered: true, Attempted: base},
		{Channel: "siren", Required: true, Delivered: true, Attempted: base.Add(2 * time.Minute)},
		{Channel: "siren", Required: true, Delivered: false, Attempted: base.Add(time.Minute)},
	}}
	latest := alarm.LatestDeliveries()
	if len(latest) != 2 {
		t.Fatalf("latest=%v", latest)
	}
	if latest[0].Channel != "duty-terminal" || latest[1].Channel != "siren" {
		t.Fatalf("not sorted by channel: %v", latest)
	}
	siren := latest[1]
	if !siren.Delivered {
		t.Fatal("expected the newest siren attempt (success) to win")
	}
	if (model.Alarm{}).LatestDeliveries() == nil {
		t.Fatal("empty expansion must be a non-nil slice")
	}
}
