package httpapi

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
)

// profileWriteMargin is added to a capture's length for the response's
// write deadline: nothing is written until the game finishes the capture.
const profileWriteMargin = 30 * time.Second

// profileChunk is the size of one write of a profile download; each write
// extends the deadline (deadlineResponseWriter).
const profileChunk = 256 << 10

// handleServerProfile serves GET /servers/{id}/profile?kind=&seconds=: it
// captures a profile of the running game and answers with the file.
func (s *Server) handleServerProfile(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	kind := apitypes.ProfileKind(q.Get("kind"))
	var seconds *int
	if q.Has("seconds") {
		n, err := strconv.Atoi(q.Get("seconds"))
		if err != nil {
			return invalidField("seconds", "seconds must be a whole number")
		}
		seconds = &n
	}
	n, err := servers.ProfileSeconds(kind, seconds)
	if err != nil {
		return serversError("server", err)
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(time.Duration(n)*time.Second + profileWriteMargin))
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		select {
		case <-s.shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()
	p, err := s.servers.Profile(ctx, id, kind, seconds, s.now())
	if err != nil {
		select {
		case <-s.shutdown:
			return errUnavailable
		default:
		}
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return r.Context().Err()
		}
		return serversError("server", err)
	}
	s.logger.Info("server profile captured", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID,
		"server_id", id, "kind", string(kind), "seconds", n, "bytes", len(p.Data))
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": p.Filename}))
	h.Set("Content-Length", strconv.Itoa(len(p.Data)))
	w.WriteHeader(http.StatusOK)
	dw := &deadlineResponseWriter{ResponseWriter: w, rc: rc}
	for data := p.Data; len(data) > 0; {
		chunk := data[:min(len(data), profileChunk)]
		if _, err := dw.Write(chunk); err != nil {
			return nil // client gone
		}
		data = data[len(chunk):]
	}
	return nil
}
