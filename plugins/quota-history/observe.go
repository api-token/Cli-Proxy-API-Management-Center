package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// usageRecord is the subset of the host's usage record this plugin reads.
// The host serializes pluginapi.UsageRecord with Go's default field names.
type usageRecord struct {
	Provider        string
	AuthID          string
	AuthIndex       string
	ResponseHeaders http.Header
}

// observedSample derives the credential-level weekly reading from the
// upstream response headers on one usage record. Only the longest window is
// used, because the dashboard pools credentials on their weekly window.
func observedSample(record usageRecord, observedAt time.Time) (Sample, bool) {
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	name := strings.TrimSpace(record.AuthID)
	headers := record.ResponseHeaders
	if name == "" || len(headers) == 0 {
		return Sample{}, false
	}
	sample := Sample{
		Name:       name,
		AuthIndex:  strings.TrimSpace(record.AuthIndex),
		Provider:   provider,
		ObservedAt: observedAt,
	}

	switch provider {
	case "claude":
		// Unified utilization is a 0..1 fraction; reset is unix seconds.
		raw, ok := headerValue(headers, "Anthropic-Ratelimit-Unified-7d-Utilization")
		if !ok {
			return Sample{}, false
		}
		utilization, errParse := strconv.ParseFloat(raw, 64)
		if errParse != nil {
			return Sample{}, false
		}
		period := 7 * 24.0
		sample.WindowID = "seven-day"
		sample.PeriodHours = &period
		sample.RemainingPercent = (1 - utilization) * 100
		if resetAt, okReset := unixHeader(headers, "Anthropic-Ratelimit-Unified-7d-Reset"); okReset {
			sample.ResetAt = &resetAt
		}
		return sample, true

	case "codex":
		// Pick whichever of primary/secondary is the longer window.
		bestMinutes := 0
		bestPrefix := ""
		for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
			raw, ok := headerValue(headers, prefix+"Window-Minutes")
			if !ok {
				continue
			}
			minutes, errParse := strconv.Atoi(raw)
			if errParse != nil || minutes <= 0 {
				continue
			}
			if _, okUsed := headerValue(headers, prefix+"Used-Percent"); !okUsed {
				continue
			}
			if minutes > bestMinutes {
				bestMinutes, bestPrefix = minutes, prefix
			}
		}
		if bestPrefix == "" {
			return Sample{}, false
		}
		rawUsed, _ := headerValue(headers, bestPrefix+"Used-Percent")
		used, errParse := strconv.ParseFloat(rawUsed, 64)
		if errParse != nil {
			return Sample{}, false
		}
		period := float64(bestMinutes) / 60
		sample.PeriodHours = &period
		switch bestMinutes {
		case 7 * 24 * 60:
			sample.WindowID = "weekly"
		case 5 * 60:
			sample.WindowID = "five-hour"
		}
		sample.RemainingPercent = 100 - used
		if resetAt, okReset := unixHeader(headers, bestPrefix+"Reset-At"); okReset {
			sample.ResetAt = &resetAt
		} else if rawAfter, okAfter := headerValue(headers, bestPrefix+"Reset-After-Seconds"); okAfter {
			if seconds, errAfter := strconv.ParseInt(rawAfter, 10, 64); errAfter == nil && seconds > 0 {
				resetAt := observedAt.Add(time.Duration(seconds) * time.Second)
				sample.ResetAt = &resetAt
			}
		}
		return sample, true
	}
	return Sample{}, false
}

// headerValue looks a header up case-insensitively: the host forwards raw
// upstream headers, which are not guaranteed to use canonical keys.
func headerValue(headers http.Header, name string) (string, bool) {
	if values, ok := headers[http.CanonicalHeaderKey(name)]; ok && len(values) > 0 {
		return strings.TrimSpace(values[len(values)-1]), true
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[len(values)-1]), true
		}
	}
	return "", false
}

func unixHeader(headers http.Header, name string) (time.Time, bool) {
	raw, ok := headerValue(headers, name)
	if !ok {
		return time.Time{}, false
	}
	seconds, errParse := strconv.ParseInt(raw, 10, 64)
	if errParse != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}
