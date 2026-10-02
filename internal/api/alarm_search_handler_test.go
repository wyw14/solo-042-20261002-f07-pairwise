package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/cry-98/internal/alarm"
	"github.com/wyw14/cry-98/internal/journal"
	"github.com/wyw14/cry-98/internal/model"
)

type stubDispatcher struct{}

func (stubDispatcher) Dispatch(context.Context, model.Alarm) ([]model.ChannelResult, error) {
	return []model.ChannelResult{
		{Channel: "siren", Required: true, Delivered: true, Attempted: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)},
	}, nil
}

type stubRetries struct{}

func (stubRetries) Schedule(model.Alarm) {}

func newSearchTestServer(t *testing.T) (*Server, *alarm.Service) {
	t.Helper()
	store, err := journal.NewStore(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	clock := func() time.Time { return time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC) }
	delivery, err := alarm.NewDeliveryService(stubDispatcher{}, stubRetries{}, store, clock)
	if err != nil {
		t.Fatalf("delivery: %v", err)
	}
	service := alarm.NewService(delivery, clock)
	server := NewServer(Dependencies{Alarms: service})
	return server, service
}

func raiseOverHTTP(t *testing.T, service *alarm.Service, vesselID string, kind model.AlarmKind) {
	t.Helper()
	if _, err := service.Raise(context.Background(), vesselID, kind, "event needs attention"); err != nil {
		t.Fatalf("raise: %v", err)
	}
}

func doSearch(t *testing.T, server *Server, target string) (int, map[string]any, []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v (%s)", target, err, recorder.Body.String())
	}
	return recorder.Code, body, recorder.Body.Bytes()
}

func TestSearchAlarmsFiltersAndPaginates(t *testing.T) {
	server, service := newSearchTestServer(t)
	raiseOverHTTP(t, service, "V-03", model.AlarmLowLevel)
	raiseOverHTTP(t, service, "V-03", model.AlarmPressure)
	raiseOverHTTP(t, service, "V-07", model.AlarmLeak)

	status, body, _ := doSearch(t, server, "/api/alarms/search?vessel_id=V-03&limit=1")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("first page items = %v", body["items"])
	}
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("next_cursor missing")
	}

	status, body, _ = doSearch(t, server, "/api/alarms/search?vessel_id=V-03&limit=1&cursor="+cursor)
	if status != http.StatusOK {
		t.Fatalf("page two status = %d", status)
	}
	pageTwo, _ := body["items"].([]any)
	if len(pageTwo) != 1 {
		t.Fatalf("page two items = %v", body["items"])
	}
	if body["next_cursor"] != nil && body["next_cursor"] != "" {
		t.Fatalf("expected final page, got cursor %v", body["next_cursor"])
	}

	firstID := items[0].(map[string]any)["id"].(string)
	secondID := pageTwo[0].(map[string]any)["id"].(string)
	if firstID == secondID {
		t.Fatal("same alarm returned on both pages")
	}

	// Other vessel filter excludes V-03 alarms; kind filter narrows further.
	status, body, _ = doSearch(t, server, "/api/alarms/search?vessel_id=V-07")
	if status != http.StatusOK || len(body["items"].([]any)) != 1 {
		t.Fatalf("vessel filter failed: %d %v", status, body)
	}
	status, body, _ = doSearch(t, server, "/api/alarms/search?kind=over_pressure")
	if status != http.StatusOK || len(body["items"].([]any)) != 1 {
		t.Fatalf("kind filter failed: %d %v", status, body)
	}
}

func TestSearchAlarmsRejectsBadInputs(t *testing.T) {
	server, service := newSearchTestServer(t)
	raiseOverHTTP(t, service, "V-03", model.AlarmLowLevel)

	cases := []struct {
		name   string
		target string
	}{
		{"garbage cursor", "/api/alarms/search?cursor=garbage-token!!"},
		{"oversize limit", "/api/alarms/search?limit=201"},
		{"non-numeric limit", "/api/alarms/search?limit=many"},
		{"bad time", "/api/alarms/search?created_from=yesterday"},
		{"inverted window", "/api/alarms/search?created_from=2026-10-01T09:00:00Z&created_to=2026-10-01T08:00:00Z"},
		{"unknown kind", "/api/alarms/search?kind=explosion"},
		{"unknown state", "/api/alarms/search?state=maybe"},
		{"unknown expand", "/api/alarms/search?expand=everything"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body, _ := doSearch(t, server, tc.target)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%v", status, body)
			}
			if body["error"] == "" {
				t.Fatal("error message missing")
			}
		})
	}
}

func TestSearchAlarmsExpansionControlsChannels(t *testing.T) {
	server, service := newSearchTestServer(t)
	raiseOverHTTP(t, service, "V-03", model.AlarmLowLevel)

	_, _, raw := doSearch(t, server, "/api/alarms/search")
	var compact struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &compact); err != nil {
		t.Fatal(err)
	}
	if _, present := compact.Items[0]["channels"]; present {
		t.Fatal("channels should be omitted without expand")
	}

	_, body, _ := doSearch(t, server, "/api/alarms/search?expand=latest_delivery")
	items := body["items"].([]any)
	channels := items[0].(map[string]any)["channels"].([]any)
	if len(channels) != 1 || channels[0].(map[string]any)["channel"] != "siren" {
		t.Fatalf("expanded channels missing latest delivery: %v", channels)
	}
}

func TestLegacyAlarmListIsUnchanged(t *testing.T) {
	server, service := newSearchTestServer(t)
	raiseOverHTTP(t, service, "V-03", model.AlarmLowLevel)

	request := httptest.NewRequest(http.MethodGet, "/api/alarms", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var body struct {
		Items []model.Alarm `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || len(body.Items[0].Channels) != 1 {
		t.Fatalf("legacy list changed shape: %+v", body)
	}
}
