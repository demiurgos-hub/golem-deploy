package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func toAPIAccount(a store.Account) apitypes.Account {
	return apitypes.Account{
		ID:        a.ID,
		Username:  a.Username,
		Role:      apitypes.Role(a.Role),
		Approved:  a.Approved,
		CreatedAt: a.CreatedAt.UTC(),
	}
}

func toAPIToken(t store.APIToken) apitypes.APIToken {
	return apitypes.APIToken{
		ID:         t.ID,
		Name:       t.Name,
		Prefix:     t.Prefix,
		CreatedAt:  t.CreatedAt.UTC(),
		LastUsedAt: utcPtr(t.LastUsedAt),
		ExpiresAt:  utcPtr(t.ExpiresAt),
	}
}

func toAPIJob(j store.Job) apitypes.Job {
	return apitypes.Job{
		ID:          j.ID,
		Kind:        apitypes.JobKind(j.Kind),
		SubjectType: apitypes.SubjectType(j.SubjectType),
		SubjectID:   j.SubjectID,
		Status:      apitypes.JobStatus(j.Status),
		Detail:      j.Detail,
		Restricted:  j.Restricted,
		CreatedBy:   j.CreatedBy,
		CreatedAt:   j.CreatedAt.UTC(),
		StartedAt:   utcPtr(j.StartedAt),
		FinishedAt:  utcPtr(j.FinishedAt),
	}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// pathID parses a positive integer path wildcard.
func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, badRequest("invalid " + name)
	}
	return id, nil
}
