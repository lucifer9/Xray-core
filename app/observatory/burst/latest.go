package burst

import "time"

// LatestResult is the newest probe sample recorded for an outbound.
type LatestResult struct {
	Time   time.Time
	RTT    time.Duration
	Failed bool
}

// LatestResults returns the newest sample of each given tag. Tags without
// any sample are omitted. Statistics from GetObservation average the whole
// sampling window, so this is the only way to see a single check's outcome.
func (o *Observer) LatestResults(tags []string) map[string]LatestResult {
	return o.hp.latestResults(tags)
}

func (h *HealthPing) latestResults(tags []string) map[string]LatestResult {
	h.access.Lock()
	defer h.access.Unlock()
	results := make(map[string]LatestResult, len(tags))
	for _, tag := range tags {
		r, ok := h.Results[tag]
		if !ok || r.rtts == nil || r.idx < 0 {
			continue
		}
		latest := r.rtts[r.idx]
		if latest.value == rttFailed {
			results[tag] = LatestResult{Time: latest.time, Failed: true}
		} else {
			results[tag] = LatestResult{Time: latest.time, RTT: latest.value}
		}
	}
	return results
}
