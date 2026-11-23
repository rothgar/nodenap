package sources

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// digestTransport retries a request with HTTP Digest (RFC 7616) or Basic
// auth when the server answers 401. Shelly Gen2+ devices use SHA-256 digest.
type digestTransport struct {
	Username, Password string
	Base               http.RoundTripper
	nc                 atomic.Uint32
}

func (t *digestTransport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

func (t *digestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only GET requests without bodies are used, so the request is safe to replay.
	resp, err := t.base().RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || t.Password == "" {
		return resp, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	retry := req.Clone(req.Context())
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		retry.SetBasicAuth(t.Username, t.Password)
	case "digest":
		auth, err := t.digestHeader(req, params)
		if err != nil {
			return nil, err
		}
		retry.Header.Set("Authorization", auth)
	default:
		return nil, fmt.Errorf("unsupported auth challenge %q", challenge)
	}
	return t.base().RoundTrip(retry)
}

func (t *digestTransport) digestHeader(req *http.Request, p map[string]string) (string, error) {
	var newHash func() hash.Hash
	algo := strings.ToUpper(p["algorithm"])
	switch algo {
	case "", "MD5":
		newHash = md5.New
		if algo == "" {
			algo = "MD5"
		}
	case "SHA-256":
		newHash = sha256.New
	default:
		return "", fmt.Errorf("unsupported digest algorithm %q", algo)
	}
	h := func(s string) string {
		x := newHash()
		x.Write([]byte(s))
		return hex.EncodeToString(x.Sum(nil))
	}
	cb := make([]byte, 8)
	if _, err := rand.Read(cb); err != nil {
		return "", err
	}
	cnonce := hex.EncodeToString(cb)

	nc := fmt.Sprintf("%08x", t.nc.Add(1))
	uri := req.URL.RequestURI()
	ha1 := h(t.Username + ":" + p["realm"] + ":" + t.Password)
	ha2 := h(req.Method + ":" + uri)
	qop := ""
	if q := p["qop"]; q != "" {
		qop = "auth"
	}
	var response string
	if qop != "" {
		response = h(ha1 + ":" + p["nonce"] + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	} else {
		response = h(ha1 + ":" + p["nonce"] + ":" + ha2)
	}
	parts := []string{
		fmt.Sprintf(`username="%s"`, t.Username),
		fmt.Sprintf(`realm="%s"`, p["realm"]),
		fmt.Sprintf(`nonce="%s"`, p["nonce"]),
		fmt.Sprintf(`uri="%s"`, uri),
		fmt.Sprintf(`algorithm=%s`, algo),
		fmt.Sprintf(`response="%s"`, response),
	}
	if qop != "" {
		parts = append(parts, "qop="+qop, "nc="+nc, fmt.Sprintf(`cnonce="%s"`, cnonce))
	}
	if o := p["opaque"]; o != "" {
		parts = append(parts, fmt.Sprintf(`opaque="%s"`, o))
	}
	return "Digest " + strings.Join(parts, ", "), nil
}

// parseChallenge splits `Digest realm="x", nonce="y", qop="auth"`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " ,")
		k, r, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		var v string
		if strings.HasPrefix(r, `"`) {
			end := strings.Index(r[1:], `"`)
			if end < 0 {
				v, rest = r[1:], ""
			} else {
				v, rest = r[1:end+1], r[end+2:]
			}
		} else {
			v, rest, _ = strings.Cut(r, ",")
		}
		params[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return scheme, params
}
