package apitypes

import "time"

// JobStatus is the lifecycle state of a job. queued and running are active;
// the others are terminal.
type JobStatus string

// Job statuses.
const (
	JobStatusQueued    JobStatus = "queued"
	JobStatusRunning   JobStatus = "running"
	JobStatusSuccess   JobStatus = "success"
	JobStatusError     JobStatus = "error"
	JobStatusCancelled JobStatus = "cancelled"
)

// SubjectType is the kind of resource a job acts on. At most one job per
// (subject_type, subject_id) is active at a time.
type SubjectType string

// Subject types.
const (
	SubjectTypeProject SubjectType = "project"
	SubjectTypeServer  SubjectType = "server"
)

// JobKind names what a job does. New kinds may be added; clients should
// display unknown kinds verbatim.
type JobKind string

// Job kinds.
const (
	JobKindProjectClone  JobKind = "project.clone"
	JobKindProjectPull   JobKind = "project.pull"
	JobKindProjectBranch JobKind = "project.branch"
	JobKindServerStart   JobKind = "server.start"
	JobKindServerStop    JobKind = "server.stop"
	JobKindServerRestart JobKind = "server.restart"
	JobKindServerDelete  JobKind = "server.delete"
)

// Job is a long-running operation. Routes that start one respond 202 with the
// Job; poll GET /api/v1/jobs/{id} or stream GET /api/v1/jobs/{id}/log?follow=1.
type Job struct {
	ID          int64       `json:"id"`
	Kind        JobKind     `json:"kind"`
	SubjectType SubjectType `json:"subject_type"`
	// SubjectID is the subject's id rendered as a string (e.g. "12").
	SubjectID string    `json:"subject_id"`
	Status    JobStatus `json:"status"`
	// Detail is a short, user-safe outcome message for terminal jobs (empty on
	// success). Full output is in the job log.
	Detail string `json:"detail"`
	// CreatedBy is the account that started the job; null for system jobs or
	// when that account was deleted.
	CreatedBy  *int64     `json:"created_by" tstype:"number | null,required"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at" tstype:"string | null,required"`
	FinishedAt *time.Time `json:"finished_at" tstype:"string | null,required"`
}

// JobList is the body of GET /api/v1/jobs, newest first.
type JobList struct {
	Items []Job `json:"items"`
}
