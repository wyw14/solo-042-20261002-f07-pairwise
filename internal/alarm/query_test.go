package alarm

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/journal"
	"github.com/wyw14/cry-98/internal/model"
)

type fakeDispatcher struct {
	results []model.ChannelResult
}

func (d *fakeDispatcher) Dispatch(_ context.Context, _ model.Alarm) ([]model.ChannelResult, error) {
	return d.results, nil
}

type noopRetries struct{}

func (noopRetries) Schedule(model.Alarm) {}

type mutableClock struct{ t time.Time }

func (c *mutableClock) now() time.Time { return c.t }
func (c *mutableClock) advance(d time.Duration) time.Time {
	c.t = c.t.Add(d)
	return c.t
}

func newQueryTestService(t *testing.T, results []model.ChannelResult) (*Service, *mutableClock) {
	t.Helper()
	store, err := journal.NewStore(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatalf("journal store: %v", err)
	}
	clock := &mutableClock{t: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}
	delivery, err := NewDeliveryService(&fakeDispatcher{results: results}, noopRetries{}, store, clock.now)
	if err != nil {
		t.Fatalf("delivery service: %v", err)
	}
	return NewService(delivery, clock.now), clock
}

func deliveredChannels() []model.ChannelResult {
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	return []model.ChannelResult{
		{Channel: "siren", Required: true, Delivered: true, Attempted: at},
		{Channel: "duty-terminal", Required: true, Delivered: true, Attempted: at},
	}
}

func raiseForTest(t *testing.T, service *Service, vesselID string, kind model.AlarmKind) model.Alarm {
	t.Helper()
	alarm, err := service.Raise(context.Background(), vesselID, kind, "check vessel")
	if err != nil {
		t.Fatalf("raise alarm: %v", err)
	}
	return alarm
}

func TestQueryFiltersByVesselKindStateAndWindow(t *testing.T) {
	service, clock := newQueryTestService(t, deliveredChannels())
	first := raiseForTest(t, service, "V-03", model.AlarmLowLevel)
	clock.advance(time.Minute)
	second := raiseForTest(t, service, "V-07", model.AlarmPressure)
	clock.advance(time.Minute)
	third := raiseForTest(t, service, "V-03", model.AlarmLeak)
	if _, err := service.Acknowledge(third.ID); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}

	cases := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"vessel", Filter{VesselID: "V-03"}, []string{first.ID.String(), third.ID.String()}},
		{"kind", Filter{Kind: model.AlarmPressure}, []string{second.ID.String()}},
		{"state", Filter{State: model.AlarmAcknowledged}, []string{third.ID.String()}},
		{"window around middle", Filter{
			CreatedFrom: first.CreatedAt.Add(time.Second),
			CreatedTo:   second.CreatedAt.Add(time.Second),
		}, []string{second.ID.String()}},
		{"vessel plus state", Filter{VesselID: "V-03", State: model.AlarmDelivered}, []string{first.ID.String()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := service.Query(Query{Filter: tc.filter, Limit: 50})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			got := make([]string, len(page.Items))
			for index, item := range page.Items {
				got[index] = item.ID.String()
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for index := range got {
				if got[index] != tc.want[index] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestQueryPaginatesWithoutDuplicatesOrGaps(t *testing.T) {
	service, clock := newQueryTestService(t, deliveredChannels())
	const total = 7
	ids := make([]string, 0, total)
	for index := 0; index < total; index++ {
		alarm := raiseForTest(t, service, "V-03", model.AlarmLowLevel)
		ids = append(ids, alarm.ID.String())
		clock.advance(time.Second)
	}

	seen := make(map[string]int)
	cursor := ""
	pageCount := 0
	for {
		page, err := service.Query(Query{Filter: Filter{VesselID: "V-03"}, Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pageCount, err)
		}
		for _, item := range page.Items {
			seen[item.ID.String()]++
		}
		pageCount++
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		if pageCount > total {
			t.Fatal("pagination did not terminate")
		}
	}
	if pageCount != 3 {
		t.Fatalf("page count = %d, want 3", pageCount)
	}
	if len(seen) != total {
		t.Fatalf("visited %d unique alarms, want %d", len(seen), total)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("alarm %s appeared %d times", id, count)
		}
	}
}

func TestQueryIsStableAcrossWritesAndStateChanges(t *testing.T) {
	service, clock := newQueryTestService(t, deliveredChannels())
	original := make([]string, 0, 5)
	for index := 0; index < 5; index++ {
		alarm := raiseForTest(t, service, "V-03", model.AlarmLowLevel)
		original = append(original, alarm.ID.String())
		clock.advance(time.Second)
	}

	first, err := service.Query(Query{Filter: Filter{VesselID: "V-03"}, Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}

	// Mutate the world mid-walk: a new alarm appears and every remaining
	// original alarm changes state out from under the walk.
	newAlarm := raiseForTest(t, service, "V-03", model.AlarmPressure)
	clock.advance(time.Second)
	for _, id := range original[2:] {
		parsed, err := uuid.Parse(id)
		if err != nil {
			t.Fatalf("parse uuid: %v", err)
		}
		if _, err := service.Clear(parsed); err != nil {
			t.Fatalf("clear: %v", err)
		}
	}

	// Continuing with the same state filter on the pinned snapshot still walks
	// every original match exactly once.
	cursor := first.NextCursor
	visited := []string{first.Items[0].ID.String(), first.Items[1].ID.String()}
	for cursor != "" {
		page, err := service.Query(Query{Filter: Filter{VesselID: "V-03"}, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("continuation: %v", err)
		}
		for _, item := range page.Items {
			visited = append(visited, item.ID.String())
		}
		cursor = page.NextCursor
	}
	if len(visited) != len(original) {
		t.Fatalf("walked %v, want %v", visited, original)
	}
	for index := range original {
		if visited[index] != original[index] {
			t.Fatalf("walk order %v, want %v", visited, original)
		}
	}
	for _, id := range visited {
		if id == newAlarm.ID.String() {
			t.Fatal("alarm inserted after page 1 leaked into the walk")
		}
	}

	// A fresh walk sees the newly written alarm.
	fresh, err := service.Query(Query{Filter: Filter{VesselID: "V-03"}, Limit: 50})
	if err != nil {
		t.Fatalf("fresh query: %v", err)
	}
	found := false
	for _, item := range fresh.Items {
		if item.ID == newAlarm.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("new alarm missing from a fresh walk")
	}
}

func TestQueryRejectsInvalidCursors(t *testing.T) {
	service, clock := newQueryTestService(t, deliveredChannels())
	raiseForTest(t, service, "V-03", model.AlarmLowLevel)
	clock.advance(time.Second)
	raiseForTest(t, service, "V-03", model.AlarmLowLevel)

	valid, err := service.Query(Query{Filter: Filter{VesselID: "V-03"}, Limit: 1})
	if err != nil {
		t.Fatalf("seed query: %v", err)
	}
	if valid.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}

	for _, raw := range []string{
		"not-base64!!",
		"eyJ2Ijo5OX0",                     // base64 json {"v":99}, unsupported version
		"eyJ2IjoxLCJ0cyI6IiIsImlkIjoiIn0", // version 1 but empty position
	} {
		if _, err := service.Query(Query{Filter: Filter{VesselID: "V-03"}, Limit: 1, Cursor: raw}); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("cursor %q: got %v, want ErrInvalidCursor", raw, err)
		}
	}

	// Cursor bound to a different filter is rejected rather than silently
	// skipping matches ahead of the position.
	if _, err := service.Query(Query{Filter: Filter{VesselID: "V-07"}, Limit: 1, Cursor: valid.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cross-filter cursor: got %v, want ErrInvalidCursor", err)
	}
}

func TestQueryRejectsExpiredCursorAfterEviction(t *testing.T) {
	service, clock := newQueryTestService(t, deliveredChannels())
	raiseForTest(t, service, "V-03", model.AlarmLowLevel)
	clock.advance(time.Second)
	raiseForTest(t, service, "V-03", model.AlarmLowLevel)
	first, err := service.Query(Query{Filter: Filter{VesselID: "V-03"}, Limit: 1})
	if err != nil {
		t.Fatalf("first query: %v", err)
	}

	// Pin enough other walks to evict the first snapshot.
	for index := 0; index < maxQuerySnapshots; index++ {
		if _, err := service.Query(Query{Filter: Filter{VesselID: "V-99"}, Limit: 1}); err != nil {
			t.Fatalf("eviction walk %d: %v", index, err)
		}
	}
	if _, err := service.Query(Query{Filter: Filter{VesselID: "V-03"}, Limit: 1, Cursor: first.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("expired cursor: got %v, want ErrInvalidCursor", err)
	}
}

func TestQueryEnforcesPageSizeBounds(t *testing.T) {
	service, _ := newQueryTestService(t, deliveredChannels())

	if _, err := service.Query(Query{Limit: MaxPageLimit + 1}); !errors.Is(err, ErrLimitTooLarge) {
		t.Fatalf("oversize limit: got %v, want ErrLimitTooLarge", err)
	}
	if _, err := service.Query(Query{Limit: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
	page, err := service.Query(Query{})
	if err != nil {
		t.Fatalf("default limit: %v", err)
	}
	if cap(page.Items) != 0 && len(page.Items) != 0 {
		t.Fatalf("empty store should return empty page")
	}
}

func TestQueryRejectsInvalidDimensions(t *testing.T) {
	service, _ := newQueryTestService(t, deliveredChannels())
	if _, err := service.Query(Query{Filter: Filter{Kind: "bogus"}}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := service.Query(Query{Filter: Filter{State: "bogus"}}); err == nil {
		t.Fatal("unknown state accepted")
	}
	from := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	to := from.Add(-time.Hour)
	if _, err := service.Query(Query{Filter: Filter{CreatedFrom: from, CreatedTo: to}}); !errors.Is(err, ErrInvalidTimeRange) {
		t.Fatalf("inverted range: got %v, want ErrInvalidTimeRange", err)
	}
}

func TestQueryOrdersEqualTimestampsByID(t *testing.T) {
	service, _ := newQueryTestService(t, deliveredChannels())
	var first, second model.Alarm
	// Raise two alarms at the same clock instant.
	first = raiseForTest(t, service, "V-03", model.AlarmLowLevel)
	second = raiseForTest(t, service, "V-03", model.AlarmLowLevel)
	if !first.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("test setup: timestamps differ %s %s", first.CreatedAt, second.CreatedAt)
	}
	page, err := service.Query(Query{Filter: Filter{VesselID: "V-03"}, Limit: 50})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("got %d items", len(page.Items))
	}
	if page.Items[0].ID.String() != min(first.ID.String(), second.ID.String()) {
		t.Fatal("equal timestamps not tie-broken by id")
	}
}

func TestRecordRetryResultRefreshesChannelsWithoutRegressingResolution(t *testing.T) {
	service, _ := newQueryTestService(t, deliveredChannels())
	alarm := raiseForTest(t, service, "V-03", model.AlarmLowLevel)

	retried := alarm.WithChannels([]model.ChannelResult{
		{Channel: "siren", Required: true, Delivered: true, Attempted: alarm.CreatedAt.Add(time.Minute)},
		{Channel: "duty-terminal", Required: true, Delivered: false, Error: "offline", Attempted: alarm.CreatedAt.Add(time.Minute)},
	}, alarm.UpdatedAt.Add(time.Minute))
	if retried.State != model.AlarmPartial {
		t.Fatalf("retry state = %s, want partial", retried.State)
	}
	service.RecordRetryResult(retried)
	got, ok := service.Get(alarm.ID)
	if !ok {
		t.Fatal("alarm missing")
	}
	if got.State != model.AlarmPartial || len(got.Channels) != 2 || got.Channels[1].Error != "offline" {
		t.Fatalf("retry result not merged: %+v", got)
	}

	acknowledged := got.Acknowledge(got.UpdatedAt.Add(time.Second))
	service.Restore(acknowledged)
	service.RecordRetryResult(retried)
	got, _ = service.Get(alarm.ID)
	if got.State != model.AlarmAcknowledged {
		t.Fatalf("acknowledged state regressed to %s", got.State)
	}
	if got.Channels[1].Error != "offline" {
		t.Fatal("latest channel result should still refresh after acknowledge")
	}
}
