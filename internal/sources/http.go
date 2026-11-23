package sources

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPCall is a resolved HTTP request (secrets already read).
type HTTPCall struct {
	Method, URL, Body  string
	Headers            map[string]string
	AuthType           string // "", "basic" or "digest"
	Username, Password string
	Insecure           bool
}

func (c *HTTPCall) client() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if c.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opted in per device
	}
	var rt http.RoundTripper = tr
	if c.AuthType == "digest" {
		rt = &digestTransport{Username: c.Username, Password: c.Password, Base: tr}
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: rt}
}

// Do sends the request and returns the body of a 2xx response.
func (c *HTTPCall) Do(ctx context.Context) ([]byte, error) {
	var body io.Reader
	if c.Body != "" {
		body = strings.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, body)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.TrimSpace(c.Body), "{") {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	if c.AuthType == "basic" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", c.Method, c.URL, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// ExtractNumber walks a dot path ("a.b.0.c") into a JSON document and returns
// the number there; numeric strings (Home Assistant states) are accepted. An
// empty path reads the whole body.
func ExtractNumber(body []byte, path string) (float64, error) {
	var cur any
	if err := json.Unmarshal(body, &cur); err != nil {
		if v, perr := strconv.ParseFloat(strings.TrimSpace(string(body)), 64); path == "" && perr == nil {
			return v, nil
		}
		return 0, fmt.Errorf("response is not JSON: %w", err)
	}
	if path != "" {
		for _, seg := range strings.Split(path, ".") {
			switch node := cur.(type) {
			case map[string]any:
				next, ok := node[seg]
				if !ok {
					return 0, fmt.Errorf("field %q: no key %q", path, seg)
				}
				cur = next
			case []any:
				i, err := strconv.Atoi(seg)
				if err != nil || i < 0 || i >= len(node) {
					return 0, fmt.Errorf("field %q: bad index %q", path, seg)
				}
				cur = node[i]
			default:
				return 0, fmt.Errorf("field %q: %q is not an object or array", path, seg)
			}
		}
	}
	switch v := cur.(type) {
	case float64:
		return v, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, fmt.Errorf("field %q: %q is not a number", path, v)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("field %q is %T, not a number", path, v)
	}
}
