package notify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

var errChannelUnavailable = errors.New("channel unavailable")

type retryTestChannel struct {
	name     string
	failures int
}

func (c *retryTestChannel) Name() string   { return c.name }
func (c *retryTestChannel) Required() bool { return true }

func (c *retryTestChannel) Send(context.Context, model.Alarm) error {
	if c.failures > 0 {
		c.failures--
		return errChannelUnavailable
	}
	return nil
}

func TestRetryQueueNotifiesResultSinkWithLatestResults(t *testing.T) {
	clock := func() time.Time { return time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC) }
	channel := &retryTestChannel{name: "siren"}
	dispatcher, err := NewDispatcher(clock, channel)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	queue := NewRetryQueue(0, clock)

	var received model.Alarm
	queue.SetResultSink(func(alarm model.Alarm) { received = alarm })

	alarm, err := model.NewAlarm("V-03", model.AlarmLowLevel, "low level", clock())
	if err != nil {
		t.Fatal(err)
	}
	alarm = alarm.WithChannels([]model.ChannelResult{
		{Channel: "siren", Required: true, Delivered: false, Error: "down", Attempted: clock()},
	}, clock())
	queue.Schedule(alarm)

	if errs := queue.RunDue(context.Background(), dispatcher); len(errs) != 0 {
		t.Fatalf("retry should succeed, got %v", errs)
	}
	if received.ID != alarm.ID {
		t.Fatal("sink not invoked with retried alarm")
	}
	if len(received.Channels) != 1 || !received.Channels[0].Delivered {
		t.Fatalf("sink received stale channel result: %+v", received.Channels)
	}
	if received.State != model.AlarmDelivered {
		t.Fatalf("state = %s, want delivered", received.State)
	}
}
