package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/wyw14/cry-98/internal/alarm"
	"github.com/wyw14/cry-98/internal/model"
)

// expandLatestDelivery is the documented expansion flag for the alarm search
// endpoint. Channel delivery results are stored on each alarm, so requesting
// the expansion includes the most recent per-channel dispatch outcome.
const expandLatestDelivery = "latest_delivery"

type alarmSearchResponse struct {
	Items      []alarmView `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// alarmView shadows the embedded alarm's always-serialized channels field so
// the latest per-channel delivery results are only present when expanded.
type alarmView struct {
	model.Alarm
	Channels []model.ChannelResult `json:"channels,omitempty"`
}

func toAlarmViews(alarms []model.Alarm, expandDelivery bool) []alarmView {
	views := make([]alarmView, len(alarms))
	for index, alarmValue := range alarms {
		views[index] = alarmView{Alarm: alarmValue}
		if expandDelivery {
			views[index].Channels = alarmValue.Channels
		}
	}
	return views
}

// searchAlarms serves GET /api/alarms/search: composite filtering over vessel,
// kind, state and creation window with stable cursor pagination. The legacy
// GET /api/alarms handler is left untouched.
func (s *Server) searchAlarms(writer http.ResponseWriter, request *http.Request) {
	values := request.URL.Query()

	query := alarm.Query{
		Filter: alarm.Filter{
			VesselID: values.Get("vessel_id"),
			Kind:     model.AlarmKind(values.Get("kind")),
			State:    model.AlarmState(values.Get("state")),
		},
		Cursor: values.Get("cursor"),
	}

	if raw := values.Get("created_from"); raw != "" {
		from, err := parseAlarmTime(raw)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid created_from: use RFC3339 time, e.g. 2026-10-01T08:00:00Z")
			return
		}
		query.CreatedFrom = from
	}
	if raw := values.Get("created_to"); raw != "" {
		to, err := parseAlarmTime(raw)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid created_to: use RFC3339 time, e.g. 2026-10-01T08:00:00Z")
			return
		}
		query.CreatedTo = to
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid limit: must be a positive integer")
			return
		}
		query.Limit = limit
	}
	switch values.Get("expand") {
	case "", expandLatestDelivery:
	default:
		writeError(writer, http.StatusBadRequest, "unknown expand value; supported: "+expandLatestDelivery)
		return
	}

	page, err := s.deps.Alarms.Query(query)
	if err != nil {
		writeAlarmQueryError(writer, err)
		return
	}
	response := alarmSearchResponse{
		Items:      toAlarmViews(page.Items, values.Get("expand") == expandLatestDelivery),
		NextCursor: page.NextCursor,
	}
	writeJSON(writer, http.StatusOK, response)
}

func writeAlarmQueryError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, alarm.ErrInvalidCursor):
		writeError(writer, http.StatusBadRequest, "invalid or expired cursor")
	case errors.Is(err, alarm.ErrLimitTooLarge),
		errors.Is(err, alarm.ErrInvalidTimeRange):
		writeError(writer, http.StatusBadRequest, err.Error())
	default:
		writeError(writer, http.StatusBadRequest, err.Error())
	}
}

func parseAlarmTime(raw string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}
