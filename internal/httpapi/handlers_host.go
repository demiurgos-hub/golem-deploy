package httpapi

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/staticfiles"
)

// Images and volumes.

func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	list, err := s.servers.Images(r.Context())
	if err != nil {
		return serversError("image", err)
	}
	writeJSON(w, http.StatusOK, apitypes.ImageList{Items: list})
	return nil
}

func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id := r.PathValue("id")
	if err := s.servers.RemoveImage(r.Context(), id); err != nil {
		if errors.Is(err, docker.ErrImageMultipleTags) {
			return conflict(apitypes.ErrorCodeConflict, docker.ErrImageMultipleTags.Error())
		}
		var ob *servers.ImageOfBuildError
		if errors.As(err, &ob) {
			return conflict(apitypes.ErrorCodeConflict, "the image belongs to build "+joinIDs(ob.BuildIDs)+"; delete the build instead (DELETE /api/v1/builds/{id})")
		}
		return serversError("image", err)
	}
	s.logger.Info("image deleted", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "image", id)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	list, err := s.servers.Volumes(r.Context())
	if err != nil {
		return serversError("volume", err)
	}
	writeJSON(w, http.StatusOK, apitypes.VolumeList{Items: list})
	return nil
}

func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	name := r.PathValue("name")
	if err := s.servers.RemoveVolume(r.Context(), name); err != nil {
		return serversError("volume", err)
	}
	s.logger.Info("volume deleted", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "volume", name)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// Static files.

func toAPIStatic(f staticfiles.File) apitypes.StaticFile {
	return apitypes.StaticFile{Name: f.Name, SizeBytes: f.SizeBytes, ModifiedAt: f.ModifiedAt, IsZip: f.IsZip}
}

func staticError(err error) error {
	var ne *staticfiles.NameError
	switch {
	case errors.As(err, &ne):
		return badRequest(ne.Msg)
	case errors.Is(err, staticfiles.ErrNotFound):
		return notFound("file")
	}
	return internal(err)
}

func (s *Server) needStatic() error {
	if s.static == nil {
		return errUnavailable
	}
	return nil
}

// staticList renders files with the store's current usage.
func (s *Server) staticList(files []staticfiles.File) (apitypes.StaticFileList, error) {
	u, err := s.static.Usage()
	if err != nil {
		return apitypes.StaticFileList{}, err
	}
	out := apitypes.StaticFileList{Items: make([]apitypes.StaticFile, 0, len(files)),
		UsedBytes: u.UsedBytes, QuotaBytes: u.QuotaBytes, FileCount: u.Files, MaxFiles: u.MaxFiles}
	for _, f := range files {
		out.Items = append(out.Items, toAPIStatic(f))
	}
	return out, nil
}

func (s *Server) handleListStatic(w http.ResponseWriter, r *http.Request) error {
	if err := s.needStatic(); err != nil {
		return err
	}
	files, err := s.static.List()
	if err != nil {
		return internal(err)
	}
	out, err := s.staticList(files)
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// deadlineReader extends the connection's read deadline as an upload
// progresses, so large uploads are bounded by inactivity, not total time.
type deadlineReader struct {
	r    io.Reader
	rc   *http.ResponseController
	last time.Time
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	if now := time.Now(); now.Sub(d.last) > time.Second {
		_ = d.rc.SetReadDeadline(now.Add(readTimeout))
		d.last = now
	}
	return d.r.Read(p)
}

func uploadError(err error) error {
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return newError(http.StatusRequestEntityTooLarge, apitypes.ErrorCodeRequestTooLarge, "upload exceeds the request size limit")
	case errors.Is(err, staticfiles.ErrTooLarge):
		return newError(http.StatusRequestEntityTooLarge, apitypes.ErrorCodeRequestTooLarge, "a file exceeds the per-file size limit")
	case errors.Is(err, staticfiles.ErrQuota):
		return newError(http.StatusInsufficientStorage, apitypes.ErrorCodeInsufficientStorage, "the upload would exceed the static store's quota (size or number of files)")
	case errors.Is(err, staticfiles.ErrNoSpace):
		return newError(http.StatusInsufficientStorage, apitypes.ErrorCodeInsufficientStorage, "not enough free disk space for the upload")
	}
	e := badRequest("the upload was interrupted or is not valid multipart data")
	e.cause = err
	return e
}

// handleUploadStatic streams multipart field "files" into the static store.
// Every file is staged first (bounded by the per-file limit, the remaining
// quota and the free-space reserve); then all are validated and committed
// together, refusing to overwrite unless ?replace=1. Each account may run
// staticfiles.DefaultUploadsPerKey uploads at once.
func (s *Server) handleUploadStatic(w http.ResponseWriter, r *http.Request) error {
	if err := s.needStatic(); err != nil {
		return err
	}
	release, ok := s.static.BeginUpload(strconv.FormatInt(identityFrom(r).Account.ID, 10))
	if !ok {
		e := newError(http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited, "too many uploads in progress for this account; wait for one to finish")
		w.Header().Set("Retry-After", "5")
		return e
	}
	defer release()
	replace := false
	switch r.URL.Query().Get("replace") {
	case "", "0", "false":
	case "1", "true":
		replace = true
	default:
		return badRequest("replace must be 1 or 0")
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		return newError(http.StatusUnsupportedMediaType, apitypes.ErrorCodeUnsupportedMediaType, "request body must be multipart/form-data")
	}
	r.Body = io.NopCloser(&deadlineReader{r: r.Body, rc: http.NewResponseController(w)})
	mr, err := r.MultipartReader()
	if err != nil {
		return badRequest("request body must be multipart/form-data")
	}
	var staged []*staticfiles.Staged
	defer func() {
		for _, st := range staged {
			st.Discard()
		}
	}()
	names := map[string]bool{}
	budget := s.static.Budget()
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return uploadError(err)
		}
		if part.FormName() != "files" {
			_ = part.Close()
			e := badRequest("unexpected form field; send files in the \"files\" field")
			e.field = clip(part.FormName(), 64)
			return e
		}
		// Read the raw file name: Part.FileName would silently strip
		// directories, hiding traversal attempts instead of rejecting them.
		_, params, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		name := params["filename"]
		if err := staticfiles.ValidateName(name); err != nil {
			_ = part.Close()
			return invalidField("files", err.Error())
		}
		key := staticfiles.NameKey(name)
		if names[key] {
			_ = part.Close()
			return invalidField("files", "the same file name appears twice in the upload")
		}
		names[key] = true
		if len(names) > staticfiles.MaxFilesPerUpload {
			_ = part.Close()
			return invalidField("files", "too many files in one upload")
		}
		st, err := s.static.Stage(name, part, staticfiles.MaxFileBytes, budget)
		_ = part.Close()
		if err != nil {
			return uploadError(err)
		}
		if budget >= 0 {
			budget -= st.Size()
		}
		staged = append(staged, st)
	}
	if len(staged) == 0 {
		return invalidField("files", "no files were uploaded")
	}
	files, err := s.commitStatic(r, staged, replace)
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return err
	}
	var ne *staticfiles.NameError
	switch {
	case errors.Is(err, staticfiles.ErrExists):
		return conflict(apitypes.ErrorCodeConflict, "a file with that name already exists; upload with replace=1 to overwrite it")
	case errors.Is(err, staticfiles.ErrQuota), errors.Is(err, staticfiles.ErrNoSpace):
		return uploadError(err)
	case errors.As(err, &ne):
		return invalidField("files", ne.Msg)
	case err != nil:
		return internal(fmt.Errorf("commit static upload: %w", err))
	}
	out, err := s.staticList(files)
	if err != nil {
		return internal(err)
	}
	s.logger.Info("static files uploaded", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "count", len(files), "replace", replace)
	writeJSON(w, http.StatusCreated, out)
	return nil
}

func (s *Server) commitStatic(r *http.Request, staged []*staticfiles.Staged, replace bool) ([]staticfiles.File, error) {
	s.proj.addonsMu.Lock()
	defer s.proj.addonsMu.Unlock()
	if replace {
		for _, file := range staged {
			users, err := s.store.ProjectsUsingStatic(r.Context(), file.Name)
			if err != nil {
				return nil, internal(err)
			}
			if err := authorizeStaticMutation(r, users); err != nil {
				return nil, err
			}
		}
	}
	return s.static.Commit(staged, replace)
}

// deadlineResponseWriter extends the write deadline per write, for large
// downloads.
type deadlineResponseWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (d *deadlineResponseWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(streamWriteWindow))
	return d.ResponseWriter.Write(p)
}

func (s *Server) handleDownloadStatic(w http.ResponseWriter, r *http.Request) error {
	if err := s.needStatic(); err != nil {
		return err
	}
	name := r.PathValue("name")
	f, info, err := s.static.OpenFile(name)
	if err != nil {
		return staticError(err)
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(&deadlineResponseWriter{ResponseWriter: w, rc: http.NewResponseController(w)}, r, "", info.ModTime(), f)
	return nil
}

func (s *Server) handleDeleteStatic(w http.ResponseWriter, r *http.Request) error {
	if err := s.needStatic(); err != nil {
		return err
	}
	name := r.PathValue("name")
	if err := staticfiles.ValidateName(name); err != nil {
		return staticError(err)
	}
	s.proj.addonsMu.Lock()
	defer s.proj.addonsMu.Unlock()
	users, err := s.store.ProjectsUsingStatic(r.Context(), name)
	if err != nil {
		return internal(err)
	}
	if err := authorizeStaticMutation(r, users); err != nil {
		return err
	}
	if len(users) > 0 {
		return conflict(apitypes.ErrorCodeConflict, "the file is a static add-on of project "+strings.Join(users, ", ")+"; remove it from the project first")
	}
	if err := s.static.Remove(name); err != nil {
		return staticError(err)
	}
	s.logger.Info("static file deleted", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "name", name)
	w.WriteHeader(http.StatusNoContent)
	return nil
}
