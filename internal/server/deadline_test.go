package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// deadlineRecorder records the read deadlines a handler sets.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

// The deadline moves with the body and is cleared at its end, so slow
// work after the body is not cut off by it.
func TestDeadlineClearedAtEndOfBody(t *testing.T) {
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := &deadlineReader{r: io.NopCloser(strings.NewReader("chunk")), rc: http.NewResponseController(w)}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	if len(w.deadlines) < 2 {
		t.Fatalf("got %d deadlines, want one per read and a final clear", len(w.deadlines))
	}
	for _, d := range w.deadlines[:len(w.deadlines)-1] {
		if d.IsZero() {
			t.Error("deadline cleared before the end of the body")
		}
	}
	if last := w.deadlines[len(w.deadlines)-1]; !last.IsZero() {
		t.Errorf("deadline still set after the body ended: %v", last)
	}
}
