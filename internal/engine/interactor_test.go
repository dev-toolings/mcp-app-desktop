package engine

import (
	"context"
	"testing"

	"github.com/go-rod/rod/lib/proto"
)

type clickRecorder struct {
	events []proto.InputDispatchMouseEvent
}

func (r *clickRecorder) Call(_ context.Context, _, _ string, params any) ([]byte, error) {
	r.events = append(r.events, params.(proto.InputDispatchMouseEvent))
	return nil, nil
}

func TestDispatchClickCarriesModifiers(t *testing.T) {
	var recorder clickRecorder
	if err := dispatchClick(&recorder, proto.Point{X: 12, Y: 34}, proto.InputMouseButtonLeft, 2, 12); err != nil {
		t.Fatal(err)
	}
	want := []proto.InputDispatchMouseEventType{
		proto.InputDispatchMouseEventTypeMouseMoved,
		proto.InputDispatchMouseEventTypeMousePressed,
		proto.InputDispatchMouseEventTypeMouseReleased,
		proto.InputDispatchMouseEventTypeMousePressed,
		proto.InputDispatchMouseEventTypeMouseReleased,
	}
	if len(recorder.events) != len(want) {
		t.Fatalf("events: %+v", recorder.events)
	}
	for i, ev := range recorder.events {
		if ev.Type != want[i] || ev.Modifiers != 12 || ev.ClickCount != (i+1)/2 {
			t.Errorf("event %d: %+v", i, ev)
		}
	}
}
