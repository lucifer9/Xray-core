package burst

import (
	"testing"
	"time"
)

func TestLatestResults(t *testing.T) {
	h := &HealthPing{Settings: &HealthPingSettings{Interval: time.Minute, SamplingCount: 3}}
	h.PutResult("a", 300*time.Millisecond)
	h.PutResult("a", 100*time.Millisecond)
	h.PutResult("b", 200*time.Millisecond)
	h.PutResult("b", rttFailed)

	got := h.latestResults([]string{"a", "b", "c"})
	if len(got) != 2 {
		t.Fatalf("expect results for a and b only, got %v", got)
	}
	if a := got["a"]; a.Failed || a.RTT != 100*time.Millisecond || a.Time.IsZero() {
		t.Errorf("expect a's newest sample 100ms, got %+v", a)
	}
	if b := got["b"]; !b.Failed || b.RTT != 0 {
		t.Errorf("expect b's newest sample to be a failure, got %+v", b)
	}
}
