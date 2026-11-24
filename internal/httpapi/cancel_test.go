package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
)

// logRecords captures structured log records for assertions.
type logRecords struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logRecords) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logRecords) all(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// find returns the records with message msg.
func (l *logRecords) find(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range l.all(t) {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func (l *logRecords) noneAtOrAbove(t *testing.T, level string) {
	t.Helper()
	for _, rec := range l.all(t) {
		if lv, _ := rec["level"].(string); lv == level || (level == "WARN" && lv == "ERROR") {
			t.Fatalf("unexpected %s record: %v", lv, rec)
		}
	}
}

func captureRecords(h *harness) *logRecords {
	rec := &logRecords{}
	h.srv.logger = slog.New(slog.NewJSONHandler(rec, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return rec
}

// failingRequest serves one request through the outer middleware to a
// handler that runs before(ctx cancel) and then fails with err, as a route
// handler whose docker or git call failed would.
func failingRequest(h *harness, cancelFirst bool, err error) *httptest.ResponseRecorder {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := h.srv.outer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cancelFirst {
			cancel() // the browser navigated away while the call ran
		}
		h.srv.writeError(w, r, internal(err))
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/servers/1/stats", nil).WithContext(ctx))
	return rr
}

func accessLog(t *testing.T, logs *logRecords) map[string]any {
	t.Helper()
	recs := logs.find(t, "http request")
	if len(recs) != 1 {
		t.Fatalf("want one access log record, got %v", recs)
	}
	return recs[0]
}

func TestClientCancelledRequestIsNotAnError(t *testing.T) {
	h := newHarness(t)
	logs := captureRecords(h)
	// What the docker layer returns when its command is killed because the
	// request context was cancelled (classify sets Kind = ctx.Err()).
	rr := failingRequest(h, true, fmt.Errorf("docker stats: %w", context.Canceled))

	logs.noneAtOrAbove(t, "WARN")
	if got := logs.find(t, "request failed"); len(got) != 0 {
		t.Fatalf("logged as a failure: %v", got)
	}
	acc := accessLog(t, logs)
	if acc["level"] != "DEBUG" || acc["status"] != float64(StatusClientClosedRequest) {
		t.Fatalf("access log %v, want DEBUG with status 499", acc)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("wrote a response to a cancelled request: %q", rr.Body.String())
	}
}

func TestClientGoneWithoutErrorIsLoggedAs499(t *testing.T) {
	h := newHarness(t)
	logs := captureRecords(h)
	ctx, cancel := context.WithCancel(context.Background())
	handler := h.srv.outer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		cancel() // e.g. DELETE /servers/{id} returns nil once the client is gone
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("DELETE", "/api/v1/servers/1", nil).WithContext(ctx))
	acc := accessLog(t, logs)
	if acc["level"] != "DEBUG" || acc["status"] != float64(StatusClientClosedRequest) {
		t.Fatalf("access log %v, want DEBUG with status 499", acc)
	}
}

func TestInternalTimeoutIsStillAnError(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("client_cancelled=%v", cancelled), func(t *testing.T) {
			h := newHarness(t)
			logs := captureRecords(h)
			// The docker layer's own timeout fired (context.DeadlineExceeded
			// from its WithTimeout child), whether or not the client also left.
			rr := failingRequest(h, cancelled, fmt.Errorf("docker image ls: %w", context.DeadlineExceeded))
			failed := logs.find(t, "request failed")
			if len(failed) != 1 || failed[0]["level"] != "ERROR" {
				t.Fatalf("timeout not logged as an error: %v", logs.all(t))
			}
			acc := accessLog(t, logs)
			if acc["status"] != float64(http.StatusInternalServerError) || acc["level"] != "INFO" {
				t.Fatalf("access log %v, want INFO with status 500", acc)
			}
			errBody(t, rr, http.StatusInternalServerError, apitypes.ErrorCodeInternal)
		})
	}
}

func TestCancellationFromElsewhereIsStillAnError(t *testing.T) {
	h := newHarness(t)
	logs := captureRecords(h)
	// A cancelled context that is not the request's (a job, a shared
	// operation) is a real failure for this request.
	failingRequest(h, false, fmt.Errorf("git ls-remote: %w", context.Canceled))
	if len(logs.find(t, "request failed")) != 1 {
		t.Fatalf("not logged as a failure: %v", logs.all(t))
	}
}

func TestShutdownCancellationIsReported(t *testing.T) {
	h := newHarness(t)
	logs := captureRecords(h)
	h.srv.beginShutdown()
	rr := failingRequest(h, true, fmt.Errorf("docker ps: %w", context.Canceled))
	warned := logs.find(t, "request aborted by daemon shutdown")
	if len(warned) != 1 || warned[0]["level"] != "WARN" {
		t.Fatalf("shutdown cancellation not reported: %v", logs.all(t))
	}
	acc := accessLog(t, logs)
	if acc["status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("access log %v, want status 503", acc)
	}
	errBody(t, rr, http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
}
