package certs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/mholt/acmez/v3/acme"
)

const (
	maxACMEResponseBytes = 2 << 20
	maxACMEOrderBytes    = 16 << 20
	maxACMEOrderRequests = 200
)

var (
	errACMEOrderBudget = errors.New("ACME order exceeded its traffic limits")
	acmeIssuerLink     = regexp.MustCompile(`<[^>]+>;\s*rel="up"`)
)

// Each Acmez operation uses its own transport wrapper. Acmez sends these
// requests synchronously, including issuer downloads and nonce retries.
type acmeTransport struct {
	base                  http.RoundTripper
	orderDeadline         time.Time
	requests              int
	bytes                 int64
	exhausted             bool
	retryAfter            time.Time
	newOrderURL           string
	rejectedReplacement   bool
	pendingAuthorizations map[string]bool
}

func (t *acmeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.exhausted || t.requests >= maxACMEOrderRequests {
		t.exhausted = true
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errACMEOrderBudget
	}
	t.requests++
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	remaining := int64(maxACMEOrderBytes) - t.bytes
	limit := min(int64(maxACMEResponseBytes), remaining)
	if resp.ContentLength > limit {
		return nil, t.bodyLimitError(limit)
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, limit))
	t.bytes += int64(len(body))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, t.bodyLimitError(limit)
		}
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	t.inspectResponse(req, resp, body)
	return resp, nil
}

func (t *acmeTransport) bodyLimitError(limit int64) error {
	if limit < maxACMEResponseBytes {
		t.exhausted = true
		return errACMEOrderBudget
	}
	return &http.MaxBytesError{Limit: limit}
}

func (t *acmeTransport) inspectResponse(req *http.Request, resp *http.Response, body []byte) {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	rawRetry := resp.Header.Get("Retry-After")
	now := time.Now()
	when := acmeRetryTime(rawRetry, now)
	t.retryAfter = time.Time{}
	if resp.StatusCode == http.StatusTooManyRequests {
		t.retryAfter = when
	} else if rawRetry != "" && mediaType == "application/problem+json" {
		var problem acme.Problem
		if json.Unmarshal(body, &problem) == nil && problem.Type == acme.ProblemTypeRateLimited {
			t.retryAfter = when
		}
	}
	if !t.orderDeadline.IsZero() && resp.StatusCode >= 200 && resp.StatusCode < 300 && !when.IsZero() {
		// Retry-After only represents whole seconds. The order context caps
		// the actual wait when less than a second remains.
		wait := max(time.Second, min(when.Sub(now), t.orderDeadline.Sub(now)))
		resp.Header.Set("Retry-After", strconv.FormatInt(int64(wait/time.Second), 10))
	}
	if mediaType == "application/pem-certificate-chain" {
		// Acmez follows both alternate chains and issuer links. We use the
		// first chain and only need the latter links.
		var issuers []string
		for _, link := range resp.Header.Values("Link") {
			issuers = append(issuers, acmeIssuerLink.FindAllString(link, -1)...)
		}
		resp.Header.Del("Link")
		for _, issuer := range issuers {
			resp.Header.Add("Link", issuer)
		}
	}
	if t.newOrderURL == "" {
		return
	}
	endpoint := req.URL.String()
	if req.Method == http.MethodPost && endpoint == t.newOrderURL {
		t.rejectedReplacement = mediaType == "application/problem+json" && orderHasReplacement(req)
		if resp.StatusCode == http.StatusCreated {
			var order acme.Order
			if json.Unmarshal(body, &order) == nil {
				for _, auth := range order.Authorizations {
					t.pendingAuthorizations[auth] = true
				}
			}
		}
	} else if t.pendingAuthorizations[endpoint] && mediaType == "application/json" {
		var auth acme.Authorization
		if json.Unmarshal(body, &auth) == nil && auth.Status != "" && auth.Status != acme.StatusPending {
			delete(t.pendingAuthorizations, endpoint)
		}
	}
}

func orderHasReplacement(req *http.Request) bool {
	var order acme.Order
	return readACMEPayload(req, &order) == nil && order.Replaces != ""
}

func readACMEPayload(req *http.Request, dst any) error {
	if req.GetBody == nil {
		return errors.New("ACME request has no replayable body")
	}
	body, err := req.GetBody()
	if err != nil {
		return err
	}
	defer body.Close()
	return decodeACMEPayload(body, dst)
}

func decodeACMEPayload(body io.Reader, dst any) error {
	var jws struct{ Payload string }
	if err := json.NewDecoder(body).Decode(&jws); err != nil {
		return err
	}
	payload, err := base64.RawURLEncoding.DecodeString(jws.Payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, dst)
}

func acmeRetryTime(raw string, now time.Time) time.Time {
	if seconds, err := strconv.ParseUint(raw, 10, 64); err == nil {
		// A larger delta is already outside every limit we enforce.
		seconds = min(seconds, uint64(24*time.Hour/time.Second))
		return now.Add(time.Duration(seconds) * time.Second)
	}
	when, _ := http.ParseTime(raw)
	return when
}
