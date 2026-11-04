package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// routePath is resolved by the host under /v0/management/, so the dashboard
// calls /v0/management/quota-history. Plugin routes cannot live under /v8.
const routePath = "quota-history"

// maxBatch bounds one POST.
const maxBatch = 500

// managementRequest mirrors pluginapi.ManagementRequest. Body arrives base64
// encoded, which encoding/json decodes into []byte.
type managementRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

// managementResponse mirrors pluginapi.ManagementResponse.
type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

type sampleInput struct {
	Name             string     `json:"name"`
	AuthIndex        string     `json:"auth_index"`
	Provider         string     `json:"provider"`
	ObservedAt       *time.Time `json:"observed_at"`
	WindowID         string     `json:"window_id"`
	PeriodHours      *float64   `json:"period_hours"`
	RemainingPercent *float64   `json:"remaining_percent"`
	ResetAt          *time.Time `json:"reset_at"`
}

func jsonResponse(status int, payload any) managementResponse {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"failed to encode response"}`)
	}
	return managementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}
}

func errorResponse(status int, message string) managementResponse {
	return jsonResponse(status, map[string]string{"error": message})
}

// handleManagement serves GET (list since T) and POST (record a batch).
func handleManagement(store *Store, req managementRequest) managementResponse {
	if store == nil {
		return errorResponse(http.StatusServiceUnavailable, "quota history store is not ready")
	}
	switch strings.ToUpper(req.Method) {
	case http.MethodGet:
		since := time.Now().Add(-Retention)
		if raw := strings.TrimSpace(req.Query.Get("since")); raw != "" {
			parsed, errParse := time.Parse(time.RFC3339, raw)
			if errParse != nil {
				return errorResponse(http.StatusBadRequest, "invalid since; expected RFC 3339")
			}
			since = parsed
		}
		samples := store.Query(since, req.Query.Get("provider"))
		if samples == nil {
			samples = []Sample{}
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"samples":         samples,
			"retention_hours": int(Retention / time.Hour),
		})

	case http.MethodPost:
		var body struct {
			Samples []sampleInput `json:"samples"`
		}
		if errDecode := json.Unmarshal(req.Body, &body); errDecode != nil {
			return errorResponse(http.StatusBadRequest, "invalid body")
		}
		if len(body.Samples) > maxBatch {
			return errorResponse(http.StatusBadRequest, "too many samples; at most 500 per request")
		}
		samples := make([]Sample, 0, len(body.Samples))
		for _, input := range body.Samples {
			if input.RemainingPercent == nil {
				continue
			}
			sample := Sample{
				Name:             input.Name,
				AuthIndex:        input.AuthIndex,
				Provider:         input.Provider,
				WindowID:         input.WindowID,
				PeriodHours:      input.PeriodHours,
				RemainingPercent: *input.RemainingPercent,
				ResetAt:          input.ResetAt,
			}
			if input.ObservedAt != nil {
				sample.ObservedAt = *input.ObservedAt
			}
			samples = append(samples, sample)
		}
		return jsonResponse(http.StatusOK, map[string]int{
			"accepted": store.Add(SourceManagement, samples...),
		})
	}
	return errorResponse(http.StatusMethodNotAllowed, "method not allowed")
}
