// Package sources reads numbers (watts, prices, state of charge) from
// Prometheus, Home Assistant and HTTP endpoints, and resolves Secrets.
package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// ErrNoData is returned when a query matched no series.
var ErrNoData = errors.New("query returned no data")

// Prometheus is a minimal instant-query client.
type Prometheus struct {
	BaseURL string
	HTTP    *http.Client
}

// NewPrometheus returns a client, or nil when baseURL is empty.
func NewPrometheus(baseURL string) *Prometheus {
	if baseURL == "" {
		return nil
	}
	return &Prometheus{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 10 * time.Second}}
}

type promResponse struct {
	Status    string `json:"status"`
	Error     string `json:"error"`
	ErrorType string `json:"errorType"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

type promVectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// Query runs an instant query and returns the sum of all returned samples
// (a scalar result is returned as-is).
func (p *Prometheus) Query(ctx context.Context, query string) (float64, error) {
	if p == nil {
		return 0, errors.New("prometheus URL not configured (spec.integrations.prometheusURL)")
	}
	u := p.BaseURL + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var pr promResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, fmt.Errorf("decoding prometheus response (HTTP %d): %w", resp.StatusCode, err)
	}
	if pr.Status != "success" {
		return 0, fmt.Errorf("prometheus query failed: %s: %s", pr.ErrorType, pr.Error)
	}
	switch pr.Data.ResultType {
	case "scalar":
		var v [2]any
		if err := json.Unmarshal(pr.Data.Result, &v); err != nil {
			return 0, err
		}
		return parseSampleValue(v[1])
	case "vector":
		var samples []promVectorSample
		if err := json.Unmarshal(pr.Data.Result, &samples); err != nil {
			return 0, err
		}
		if len(samples) == 0 {
			return 0, ErrNoData
		}
		var sum float64
		for _, s := range samples {
			v, err := parseSampleValue(s.Value[1])
			if err != nil {
				return 0, err
			}
			sum += v
		}
		return sum, nil
	default:
		return 0, fmt.Errorf("unsupported prometheus result type %q", pr.Data.ResultType)
	}
}

func parseSampleValue(v any) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("unexpected sample value %v", v)
	}
	return strconv.ParseFloat(s, 64)
}

// RenderQuery expands {{ .NodeName }} in a query template.
func RenderQuery(tmpl, nodeName string) (string, error) {
	if !strings.Contains(tmpl, "{{") {
		return tmpl, nil
	}
	t, err := template.New("q").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, map[string]string{"NodeName": nodeName}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
