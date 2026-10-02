package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/alarm"
	"github.com/wyw14/cry-98/internal/model"
)

func newQueryTestServer(t *testing.T) (*Server, *alarm.Service) {
	t.Helper()
	service := alarm.NewService(nil, func() time.Time { return time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC) })
	server := NewServer(Dependencies{Alarms: service})
	return server, service
}

func seedViaRestore(t *testing.T, service *alarm.Service, vesselID string, kind model.AlarmKind, state model.AlarmState, at time.Time, channels ...model.ChannelResult) {
	t.Helper()
	service.Restore(model.Alarm{
		ID:        uuid.Must(uuid.NewV7()),
		VesselID:  vesselID,
		Kind:      kind,
		State:     state,
		Message:   "test alarm",
		Channels:  channels,
		CreatedAt: at.UTC(),
		UpdatedAt: at.UTC(),
	})
}

func doGet(t *testing.T, server *Server, target string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	var body map[string]any
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not json: %s", recorder.Body.String())
		}
	}
	return recorder.Code, body
}

func TestQueryAlarmsEndpointFiltersAndPaginates(t *testing.T) {
	server, service := newQueryTestServer(t)
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	for i := range 5 {
		state := model.AlarmDetected
		if i%2 == 0 {
			state = model.AlarmPartial
		}
		seedViaRestore(t, service, "V-03", model.AlarmPressure, state, base.Add(time.Duration(i)*time.Minute))
	}
	seedViaRestore(t, service, "V-07", model.AlarmLeak, model.AlarmCleared, base.Add(10*time.Minute))

	status, body := doGet(t, server, "/api/alarms/query?vessel_id=V-03&kind=over_pressure&state=partial&page_size=2")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items=%v", items)
	}
	first := items[0].(map[string]any)
	if first["vessel_id"] != "V-03" || first["kind"] != "over_pressure" || first["state"] != "partial" {
		t.Fatalf("unexpected first item %v", first)
	}
	if deliveries, ok := first["latest_deliveries"].([]any); !ok || len(deliveries) != 0 {
		t.Fatalf("expected empty latest_deliveries array, got %v", first["latest_deliveries"])
	}
	if body["has_more"] != true || body["next_cursor"] == "" {
		t.Fatalf("expected has_more with cursor, got %v", body)
	}

	// Second page walks the cursor and returns the last matching alarm.
	cursor := body["next_cursor"].(string)
	status, pageTwo := doGet(t, server, "/api/alarms/query?vessel_id=V-03&kind=over_pressure&state=partial&page_size=2&cursor="+cursor)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, pageTwo)
	}
	if len(pageTwo["items"].([]any)) != 1 || pageTwo["has_more"] != false {
		t.Fatalf("page two=%v", pageTwo)
	}
	if _, present := pageTwo["next_cursor"]; present {
		t.Fatal("final page must omit next_cursor")
	}
}

func TestQueryAlarmsEndpointExpandsLatestDeliveries(t *testing.T) {
	server, service := newQueryTestServer(t)
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	seedViaRestore(t, service, "V-03", model.AlarmPressure, model.AlarmPartial, base,
		model.ChannelResult{Channel: "siren", Required: true, Delivered: false, Error: "boom", Attempted: base},
		model.ChannelResult{Channel: "siren", Required: true, Delivered: true, Attempted: base.Add(2 * time.Minute)},
		model.ChannelResult{Channel: "duty-terminal", Required: true, Delivered: true, Attempted: base},
	)
	status, body := doGet(t, server, "/api/alarms/query")
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	item := body["items"].([]any)[0].(map[string]any)
	deliveries := item["latest_deliveries"].([]any)
	if len(deliveries) != 2 {
		t.Fatalf("deliveries=%v", deliveries)
	}
	siren := deliveries[1].(map[string]any)
	if siren["channel"] != "siren" || siren["delivered"] != true {
		t.Fatalf("expected newest siren attempt to win: %v", siren)
	}
}

func TestQueryAlarmsEndpointRejectsBadInput(t *testing.T) {
	server, _ := newQueryTestServer(t)
	cases := map[string]string{
		"/api/alarms/query?page_size=0":      "page_size",
		"/api/alarms/query?page_size=201":    "page_size",
		"/api/alarms/query?page_size=nope":   "page_size",
		"/api/alarms/query?kind=bogus":       "kind",
		"/api/alarms/query?state=bogus":      "state",
		"/api/alarms/query?created_after=x":  "created_after",
		"/api/alarms/query?cursor=garbage.x": "cursor",
	}
	for target, marker := range cases {
		status, body := doGet(t, server, target)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status=%d want 400", target, status)
		}
		message, _ := body["error"].(string)
		if message == "" {
			t.Fatalf("%s: missing error message", target)
		}
		_ = marker
	}
}

func TestLegacyAlarmListEndpointStillWorks(t *testing.T) {
	server, service := newQueryTestServer(t)
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	seedViaRestore(t, service, "V-03", model.AlarmPressure, model.AlarmDetected, base)
	seedViaRestore(t, service, "V-07", model.AlarmLeak, model.AlarmCleared, base.Add(time.Minute))

	status, body := doGet(t, server, "/api/alarms")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("legacy list must keep returning all alarms, got %v", body)
	}
	if _, present := body["next_cursor"]; present {
		t.Fatal("legacy response shape must not gain pagination fields")
	}
}
