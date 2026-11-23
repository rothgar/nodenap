package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HomeAssistant reads entity states through the REST API.
type HomeAssistant struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewHomeAssistant returns a client, or nil when baseURL is empty.
func NewHomeAssistant(baseURL, token string) *HomeAssistant {
	if baseURL == "" {
		return nil
	}
	return &HomeAssistant{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

type haState struct {
	State      string         `json:"state"`
	Attributes map[string]any `json:"attributes"`
}

// State returns an entity's numeric state, or a numeric attribute.
func (h *HomeAssistant) State(ctx context.Context, entity, attribute string) (float64, error) {
	if h == nil {
		return 0, errors.New("home assistant URL not configured (spec.integrations.homeAssistantURL)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/api/states/"+url.PathEscape(entity), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("home assistant %s: HTTP %d", entity, resp.StatusCode)
	}
	var st haState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return 0, err
	}
	if attribute == "" {
		v, err := strconv.ParseFloat(st.State, 64)
		if err != nil {
			return 0, fmt.Errorf("home assistant %s state %q is not numeric", entity, st.State)
		}
		return v, nil
	}
	switch v := st.Attributes[attribute].(type) {
	case float64:
		return v, nil
	case string:
		return strconv.ParseFloat(v, 64)
	default:
		return 0, fmt.Errorf("home assistant %s attribute %q is not numeric", entity, attribute)
	}
}
