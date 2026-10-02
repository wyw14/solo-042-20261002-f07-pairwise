package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/wyw14/cry-98/internal/alarm"
	"github.com/wyw14/cry-98/internal/model"
)

const (
	defaultAlarmPageSize = 50
)

type alarmQueryItem struct {
	model.Alarm
	LatestDeliveries []model.ChannelResult `json:"latest_deliveries"`
}

type alarmQueryResponse struct {
	Items      []alarmQueryItem `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
	HasMore    bool             `json:"has_more"`
}

// queryAlarms serves GET /api/alarms/query with combined filters
// (vessel_id, kind, state, created_after, created_before), stable keyset
// cursor pagination and a latest-per-channel delivery expansion.
func (s *Server) queryAlarms(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	filter := alarm.Filter{
		VesselID: query.Get("vessel_id"),
		Kind:     model.AlarmKind(query.Get("kind")),
		State:    model.AlarmState(query.Get("state")),
	}
	if raw := query.Get("created_after"); raw != "" {
		parsed, err := parseAlarmTime(raw)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid created_after: use RFC3339 timestamp")
			return
		}
		filter.CreatedAfter = parsed
	}
	if raw := query.Get("created_before"); raw != "" {
		parsed, err := parseAlarmTime(raw)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid created_before: use RFC3339 timestamp")
			return
		}
		filter.CreatedBefore = parsed
	}
	pageSize := defaultAlarmPageSize
	if raw := query.Get("page_size"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid page_size")
			return
		}
		pageSize = parsed
	}

	page, err := s.deps.Alarms.Query(filter, pageSize, query.Get("cursor"))
	if err != nil {
		switch {
		case errors.Is(err, alarm.ErrInvalidCursor), errors.Is(err, alarm.ErrCursorFilterChanged):
			writeError(writer, http.StatusBadRequest, err.Error())
		case errors.Is(err, alarm.ErrPageSize):
			writeError(writer, http.StatusBadRequest, err.Error())
		case errors.Is(err, alarm.ErrInvalidFilter):
			writeError(writer, http.StatusBadRequest, err.Error())
		default:
			writeError(writer, http.StatusInternalServerError, "alarm query failed")
		}
		return
	}

	response := alarmQueryResponse{
		Items:      make([]alarmQueryItem, 0, len(page.Items)),
		NextCursor: page.NextCursor,
		HasMore:    page.HasMore,
	}
	for _, value := range page.Items {
		response.Items = append(response.Items, alarmQueryItem{
			Alarm:            value,
			LatestDeliveries: value.LatestDeliveries(),
		})
	}
	writeJSON(writer, http.StatusOK, response)
}

func parseAlarmTime(raw string) (time.Time, error) {
	return time.Parse(time.RFC3339, raw)
}
