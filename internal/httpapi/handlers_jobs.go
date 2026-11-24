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
// returned Job. A job started from an admin route (or from a request that
// did not come through the route table) is restricted: only admins can read
// its log or cancel it.
func (s *Server) startJob(r *http.Request, kind apitypes.JobKind, subjectType apitypes.SubjectType, subjectID int64, fn jobs.Func, opts ...jobs.StartOption) (apitypes.Job, error) {
	var createdBy *int64
	if id := identityFrom(r); id != nil {
		createdBy = &id.Account.ID
	}
	access, known := routeAccessFrom(r)
	restricted := !known || access == AccessAdmin
	job, err := s.jobs.Start(r.Context(), string(kind), jobs.Subject{Type: string(subjectType), ID: jobs.SubjectID(subjectID)}, createdBy, fn, append([]jobs.StartOption{jobs.Restricted(restricted)}, opts...)...)
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

// jobAction is something a caller may do with a job.
type jobAction int

const (
	jobView jobAction = iota + 1
	jobReadLog
	jobCancel
)

// authorizeJob is the single per-job permission check (the route table
// only decides who may reach the job routes at all). Admin sessions may do
// anything. Restricted jobs are otherwise off limits. The log of any other
// job is readable by every developer; cancelling it needs its creator.
func authorizeJob(id *Identity, j store.Job, action jobAction) error {
	if id.IsAdmin() {
		return nil
	}
	if j.SubjectType == string(apitypes.SubjectTypeCertificate) {
		return errForbidden
	}
	if j.Restricted && action != jobView {
		return newError(http.StatusForbidden, apitypes.ErrorCodeForbidden, "this job was started by an administrator action; only admins can read its log or cancel it")
	}
	switch action {
	case jobView, jobReadLog:
		return nil
	case jobCancel:
		if j.CreatedBy != nil && *j.CreatedBy == id.Account.ID {
			return nil
		}
		return newError(http.StatusForbidden, apitypes.ErrorCodeForbidden, "only the account that started this job or an admin can cancel it")
	}
	return errForbidden
}

// acquireLogStream reserves one of the caller's concurrent following log
// streams (job logs, and server logs once implemented). The returned
// release must be deferred.
func (s *Server) acquireLogStream(r *http.Request) (func(), error) {
	release, ok := s.logStreams.Acquire(strconv.FormatInt(identityFrom(r).Account.ID, 10))
	if !ok {
		e := newError(http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited, "too many open log streams; close one and retry")
		e.header = http.Header{"Retry-After": {"5"}}
		return nil, e
	}
	return release, nil
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) error {
	j, err := s.loadJob(r)
	if err != nil {
		return err
	}
	if err := authorizeJob(identityFrom(r), j, jobView); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toAPIJob(j))
	return nil
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f := store.JobFilter{SubjectType: q.Get("subject_type"), SubjectID: q.Get("subject_id"), Limit: 50}
	if !identityFrom(r).IsAdmin() {
		f.ExcludeSubjectType = string(apitypes.SubjectTypeCertificate)
	}
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
	if err := authorizeJob(identityFrom(r), j, jobCancel); err != nil {
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
// disconnects or the daemon shuts down; each account may hold a few such
// streams at once.
func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) error {
	j, err := s.loadJob(r)
	if err != nil {
		return err
	}
	if err := authorizeJob(identityFrom(r), j, jobReadLog); err != nil {
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
	if follow {
		release, err := s.acquireLogStream(r)
		if err != nil {
			return err
		}
		defer release()
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
