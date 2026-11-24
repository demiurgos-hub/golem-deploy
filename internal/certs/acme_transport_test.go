package certs

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type acmeRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f acmeRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestACMEPollingClampsRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		header   string
		min, max int
	}{
		{"0", 1, 1},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 1, 1},
		{"3600", 8, 10},
		{"3", 3, 3},
	} {
		t.Run(tc.header, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://ca.example/auth", nil)
			transport := acmeTransport{orderDeadline: time.Now().Add(10 * time.Second), base: acmeRoundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Retry-After": {tc.header}},
					Body: io.NopCloser(strings.NewReader(`{"status":"pending"}`))}, nil
			})}
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || seconds < tc.min || seconds > tc.max {
				t.Fatalf("unbounded poll Retry-After: %q", resp.Header.Get("Retry-After"))
			}
		})
	}
}

func TestACMEOrderHasAggregateTrafficLimits(t *testing.T) {
	for _, tc := range []struct {
		name           string
		size, requests int
	}{
		{"requests", 0, maxACMEOrderRequests},
		{"bytes", maxACMEResponseBytes, maxACMEOrderBytes / maxACMEResponseBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://ca.example/order", nil)
			data := strings.Repeat("x", tc.size)
			transport := acmeTransport{base: acmeRoundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(data)), ContentLength: -1}, nil
			})}
			for range tc.requests {
				resp, err := transport.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, resp.Body); err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			if _, err := transport.RoundTrip(req); !errors.Is(err, errACMEOrderBudget) {
				t.Fatalf("operation exceeded %s limit: %v", tc.name, err)
			}
		})
	}
	t.Run("coincident response limit", func(t *testing.T) {
		transport := acmeTransport{
			bytes: maxACMEOrderBytes - maxACMEResponseBytes,
			base: acmeRoundTripperFunc(func(*http.Request) (*http.Response, error) {
				body := strings.NewReader(strings.Repeat("x", maxACMEResponseBytes+1))
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), ContentLength: -1}, nil
			}),
		}
		if _, err := transport.RoundTrip(httptest.NewRequest(http.MethodPost, "http://ca.example/order", nil)); !errors.Is(err, errACMEOrderBudget) {
			t.Fatalf("overlapping response and order limits must end the order: %v", err)
		}
	})
}

func TestACMEOnlyFollowsIssuerLinksForFirstChain(t *testing.T) {
	const issuer = `<https://ca.example/issuer>; rel="up"`
	transport := acmeTransport{base: acmeRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		header := http.Header{"Content-Type": {"application/pem-certificate-chain"}}
		header.Add("Link", `<https://ca.example/alternate>; rel="alternate", `+issuer)
		header.Add("Link", `<https://ca.example/another>; rel="alternate"`)
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: http.NoBody}, nil
	})}
	resp, err := transport.RoundTrip(httptest.NewRequest(http.MethodPost, "http://ca.example/cert", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if links := resp.Header.Values("Link"); len(links) != 1 || links[0] != issuer {
		t.Fatalf("unexpected links exposed to Acmez: %v", links)
	}
}
