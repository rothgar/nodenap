package sources

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

// Resolver reads Secrets and evaluates Values.
type Resolver struct {
	// Reader for Secrets. Use an uncached reader so the scaler never
	// watches (or needs RBAC for) every Secret in the cluster.
	Reader    client.Reader
	Namespace string // default namespace for secret refs
	Prom      *Prometheus
	HA        *HomeAssistant
}

// Secret returns a Secret's data.
func (r *Resolver) Secret(ctx context.Context, name, namespace string) (map[string][]byte, error) {
	if namespace == "" {
		namespace = r.Namespace
	}
	var s corev1.Secret
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &s); err != nil {
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, err)
	}
	return s.Data, nil
}

// SecretKey returns one key of a Secret.
func (r *Resolver) SecretKey(ctx context.Context, ref v1alpha1.SecretKeyRef, defaultKey string) (string, error) {
	data, err := r.Secret(ctx, ref.Name, ref.Namespace)
	if err != nil {
		return "", err
	}
	key := ref.Key
	if key == "" {
		key = defaultKey
	}
	v, ok := data[key]
	if !ok {
		return "", fmt.Errorf("secret %s has no key %q", ref.Name, key)
	}
	return string(v), nil
}

// Credentials returns "username" and "password" from a Secret.
func (r *Resolver) Credentials(ctx context.Context, ref v1alpha1.SecretRef) (user, pass string, err error) {
	data, err := r.Secret(ctx, ref.Name, ref.Namespace)
	if err != nil {
		return "", "", err
	}
	return string(data["username"]), string(data["password"]), nil
}

// HTTPCall resolves an HTTPRequest's secrets.
func (r *Resolver) HTTPCall(ctx context.Context, req *v1alpha1.HTTPRequest, defaultMethod string) (*HTTPCall, error) {
	c := &HTTPCall{Method: req.Method, URL: req.URL, Body: req.Body, Insecure: req.InsecureSkipTLSVerify, Headers: map[string]string{}}
	if c.Method == "" {
		c.Method = defaultMethod
	}
	for k, v := range req.Headers {
		c.Headers[k] = v
	}
	if req.HeadersSecretRef != nil {
		data, err := r.Secret(ctx, req.HeadersSecretRef.Name, req.HeadersSecretRef.Namespace)
		if err != nil {
			return nil, err
		}
		for k, v := range data {
			c.Headers[k] = string(v)
		}
	}
	if req.Auth != nil {
		user, pass, err := r.Credentials(ctx, req.Auth.SecretRef)
		if err != nil {
			return nil, err
		}
		if user == "" {
			user = "admin" // Shelly's fixed username
		}
		c.AuthType, c.Username, c.Password = req.Auth.Type, user, pass
	}
	return c, nil
}

// ReadHTTP performs an HTTP reading: GET, then Field and Scale.
func (r *Resolver) ReadHTTP(ctx context.Context, req *v1alpha1.HTTPRequest) (float64, error) {
	c, err := r.HTTPCall(ctx, req, "GET")
	if err != nil {
		return 0, err
	}
	body, err := c.Do(ctx)
	if err != nil {
		return 0, err
	}
	v, err := ExtractNumber(body, req.Field)
	if err != nil {
		return 0, err
	}
	return scaled(v, req.Scale)
}

// Eval reads a Value. nodeName fills {{ .NodeName }} in Prometheus queries.
func (r *Resolver) Eval(ctx context.Context, v *v1alpha1.Value, nodeName string) (float64, error) {
	var x float64
	var err error
	switch {
	case v.Prometheus != nil:
		x, err = r.Query(ctx, v.Prometheus.Query, nodeName)
	case v.HomeAssistant != nil:
		x, err = r.haSum(ctx, v.HomeAssistant)
	case v.HTTP != nil:
		x, err = r.ReadHTTP(ctx, v.HTTP)
	case v.Static != "":
		x, err = strconv.ParseFloat(strings.TrimSpace(v.Static), 64)
	default:
		return 0, errors.New("value has no source")
	}
	if err != nil {
		return 0, err
	}
	return scaled(x, v.Scale)
}

// Query runs a (node-templated) PromQL query.
func (r *Resolver) Query(ctx context.Context, query, nodeName string) (float64, error) {
	q, err := RenderQuery(query, nodeName)
	if err != nil {
		return 0, err
	}
	return r.Prom.Query(ctx, q)
}

func (r *Resolver) haSum(ctx context.Context, s *v1alpha1.HomeAssistantSource) (float64, error) {
	ids := s.Entities
	if s.Entity != "" {
		ids = append([]string{s.Entity}, ids...)
	}
	if len(ids) == 0 {
		return 0, errors.New("homeAssistant value has no entity")
	}
	var sum float64
	for _, id := range ids {
		v, err := r.HA.State(ctx, id, s.Attribute)
		if err != nil {
			return 0, err
		}
		sum += v
	}
	return sum, nil
}

func scaled(v float64, scale string) (float64, error) {
	if strings.TrimSpace(scale) == "" {
		return v, nil
	}
	m, err := strconv.ParseFloat(strings.TrimSpace(scale), 64)
	if err != nil {
		return 0, fmt.Errorf("scale %q: %w", scale, err)
	}
	return v * m, nil
}
