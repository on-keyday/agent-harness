package cli

import "testing"

// While a start waits for a client (after a reconnect), a stop or a newer start
// for the same pane key must win. Open X, close, open Y on key "preview": X's
// reservation is superseded no matter which of the two finishes waiting first.
func TestPreviewReservationSupersededDuringWait(t *testing.T) {
	const key = "t-preview"
	defer StopPreview(key)

	x := ReservePreview(key)
	StopPreview(key)
	y := ReservePreview(key)
	if PreviewReserved(key, x) {
		t.Fatal("X still reserved after a stop and a newer start for the same key")
	}
	if !PreviewReserved(key, y) {
		t.Fatal("Y (the newest start) lost its reservation")
	}
}

func TestPreviewReservationSupersededByStopAlone(t *testing.T) {
	const key = "t-grid"
	g := ReservePreview(key)
	StopPreview(key) // a grid pane dismissed while its start waits
	if PreviewReserved(key, g) {
		t.Fatal("a dismissed pane's start is still reserved: it would attach a stream nobody reads")
	}
}
