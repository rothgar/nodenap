package v1alpha1

// SecretRef names a Secret. An empty namespace means the controller's own.
type SecretRef struct {
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// SecretKeyRef names one key of a Secret.
type SecretKeyRef struct {
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Key string `json:"key,omitempty"`
}

// Value yields one number from wherever it lives. Set exactly one source.
type Value struct {
	// PromQL instant query; the sum of returned samples is used.
	// +optional
	Prometheus *PrometheusSource `json:"prometheus,omitempty"`
	// Home Assistant entity state (or attribute). Several entities are summed.
	// +optional
	HomeAssistant *HomeAssistantSource `json:"homeAssistant,omitempty"`
	// A JSON HTTP endpoint.
	// +optional
	HTTP *HTTPRequest `json:"http,omitempty"`
	// A fixed number (decimal string).
	// +optional
	Static string `json:"static,omitempty"`
	// Multiplier applied to the value, e.g. "1000" for kW → W or "-1" to
	// flip a sign. Decimal string.
	// +optional
	Scale string `json:"scale,omitempty"`
}

// PrometheusSource is a PromQL instant query. In Machine sensors
// {{ .NodeName }} is replaced with the node name.
type PrometheusSource struct {
	Query string `json:"query"`
}

// HomeAssistantSource reads Home Assistant entity states.
type HomeAssistantSource struct {
	// +optional
	Entity string `json:"entity,omitempty"`
	// Summed with Entity, e.g. one power sensor per battery.
	// +optional
	Entities []string `json:"entities,omitempty"`
	// Read this attribute instead of the state.
	// +optional
	Attribute string `json:"attribute,omitempty"`
}

// HTTPRequest is one HTTP call. As a sensor or Value it is a GET whose JSON
// Field holds the number; as a power action it is sent as-is.
type HTTPRequest struct {
	// Defaults to POST for actions and GET for readings.
	// +optional
	Method string `json:"method,omitempty"`
	URL    string `json:"url"`
	// +optional
	Body string `json:"body,omitempty"`
	// +optional
	Headers map[string]string `json:"headers,omitempty"`
	// Every key of this Secret is sent as a header (e.g. Authorization).
	// +optional
	HeadersSecretRef *SecretRef `json:"headersSecretRef,omitempty"`
	// +optional
	Auth *HTTPAuth `json:"auth,omitempty"`
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
	// For readings: dot path to the number in the JSON response, e.g.
	// "apower" or "meters.0.power". Empty: the whole body is the number.
	// +optional
	Field string `json:"field,omitempty"`
	// For readings: multiplier (decimal string).
	// +optional
	Scale string `json:"scale,omitempty"`
}

// HTTPAuth is basic or digest auth from a Secret with "username" (optional,
// default "admin") and "password".
type HTTPAuth struct {
	// +kubebuilder:validation:Enum=basic;digest
	Type      string    `json:"type"`
	SecretRef SecretRef `json:"secretRef"`
}
