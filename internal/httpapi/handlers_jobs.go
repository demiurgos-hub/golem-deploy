package httpapi

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// startJob starts a job on behalf of the request's identity and maps runner
// errors to API errors. Handlers that start jobs respond 202 with the
// returned Job.
func (s *Server) startJob(r *http.Request, kind apitypes.JobKind, subjectType apitypes.SubjectType, subjectID int64, fn jobs.Func) (apitypes.Job, error) {
	var createdBy *int64
	if id := identityFrom(r); id != nil {
		createdBy = &id.Account.ID
	}
	job, err := s.jobs.Start(r.Context(), string(kind), jobs.Subject{Type: string(subjectType), ID: jobs.SubjectID(subjectID)}, createdBy, fn)
	switch {
	case errors.Is(err, jobs.ErrSubjectBusy):
		return apitypes.Job{}, conflict(apitypes.ErrorCodeJobInProgress, "another job is already running for this "+string(subjectType))
	case errors.Is(err, jobs.ErrShuttingDown):
		return apitypes.Job{}, errUnavailable
	case err != nil:
		return apitypes.Job{}, internal(err)
	}
	return toAPIJob(job), nil
}

func (s *Server) loadJob(r *http.Request) (store.Job, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return store.Job{}, err
	}
	j, err := s.store.GetJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Job{}, notFound("job")
	}
	if err != nil {
		return store.Job{}, internal(err)
	}
	return j, nil
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) error {
	j, err := s.loadJob(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toAPIJob(j))
	return nil
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f := store.JobFilter{SubjectType: q.Get("subject_type"), SubjectID: q.Get("subject_id"), Limit: 50}
	if len(f.SubjectType) > 32 || len(f.SubjectID) > 64 {
		return badRequest("subject filter too long")
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			return badRequest("limit must be an integer from 1 to 500")
		}
		f.Limit = n
	}
	list, err := s.store.ListJobs(r.Context(), f)
	if err != nil {
		return internal(err)
	}
	out := apitypes.JobList{Items: make([]apitypes.Job, 0, len(list))}
	for _, j := range list {
		out.Items = append(out.Items, toAPIJob(j))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) error {
	j, err := s.loadJob(r)
	if err != nil {
		return err
	}
	if j.Status.Terminal() {
		return conflict(apitypes.ErrorCodeJobNotActive, "job has already finished")
	}
	if err := s.jobs.Cancel(j.ID); err != nil {
		if errors.Is(err, jobs.ErrNotActive) {
			return conflict(apitypes.ErrorCodeJobNotActive, "job has already finished")
		}
		return internal(err)
	}
	s.logger.Info("job cancel requested", "request_id", requestIDFrom(r.Context()), "job_id", j.ID, "account_id", identityFrom(r).Account.ID)
	writeJSON(w, http.StatusAccepted, toAPIJob(j))
	return nil
}

// Streaming parameters for log responses.
const (
	streamWriteWindow = 30 * time.Second
	followPoll        = 250 * time.Millisecond
)

// handleJobLog returns the job log as text. With ?follow=1 it keeps the
// response open, sending appended output until the job ends, the client
// disconnects or the daemon shuts down.
func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) error {
	j, err := s.loadJob(r)
	if err != nil {
		return err
	}
	follow := false
	switch r.URL.Query().Get("follow") {
	case "", "0", "false":
	case "1", "true":
		follow = true
	default:
		return badRequest("follow must be 1 or 0")
	}
	// Subscribe before reading so a job that ends meanwhile is not missed.
	done := s.jobs.Done(j.ID)

	f, err := os.Open(s.jobs.LogPath(j.ID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return internal(err)
	}
	if f != nil {
		defer f.Close()
	}

	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)

	// drain copies everything currently in the file; it reports false when
	// the client is gone.
	drain := func() bool {
		if f == nil {
			return true
		}
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				_ = rc.SetWriteDeadline(time.Now().Add(streamWriteWindow))
				if _, werr := w.Write(buf[:n]); werr != nil {
					return false
				}
				if follow {
					_ = rc.Flush()
				}
			}
			if rerr == io.EOF {
				return true
			}
			if rerr != nil {
				s.logger.Warn("read job log failed", "request_id", requestIDFrom(r.Context()), "job_id", j.ID, "err", rerr)
				return false
			}
		}
	}

	if !drain() || !follow {
		return nil
	}
	_ = rc.Flush()
	ticker := time.NewTicker(followPoll)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			drain()
			return nil
		case <-r.Context().Done():
			return nil
		case <-s.shutdown:
			drain()
			return nil
		case <-ticker.C:
			if f == nil {
				f, _ = os.Open(s.jobs.LogPath(j.ID))
				if f != nil {
					defer f.Close()
				}
			}
			if !drain() {
				return nil
			}
		}
	}
}
