import { useEffect, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, ChevronDown, ChevronRight, CircleSlash, Clock, RotateCw, XCircle } from "lucide-react";
import { useApi } from "@/app/services";
import { invalidateJobSubject, isJobActive, useAccount, useJob, useSubjectJobs } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Job, JobStatus as JobStatusValue, SubjectType } from "@/api/types.gen";
import { useLogStream } from "@/hooks/use-log-stream";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { LogViewer } from "@/components/ui/log-viewer";
import { Spinner } from "@/components/ui/spinner";
import { EmptyState, ErrorState, ErrorText, LoadingState } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, formatRelative } from "@/lib/format";

const KIND_LABELS: Record<string, string> = {
  "project.clone": "Clone",
  "project.pull": "Pull",
  "project.branch": "Switch branch",
  "server.start": "Start",
  "server.stop": "Stop",
  "server.restart": "Restart",
};

/** Known kinds get a label; unknown kinds are shown verbatim, as the contract asks. */
export function jobKindLabel(kind: string): string {
  return KIND_LABELS[kind] ?? kind;
}

export function JobStatus({ status }: { status: JobStatusValue }) {
  switch (status) {
    case "queued":
      return (
        <Badge tone="neutral">
          <Clock aria-hidden />
          Queued
        </Badge>
      );
    case "running":
      return (
        <Badge tone="info">
          <Spinner className="size-3" />
          Running
        </Badge>
      );
    case "success":
      return (
        <Badge tone="success">
          <CheckCircle2 aria-hidden />
          Succeeded
        </Badge>
      );
    case "error":
      return (
        <Badge tone="danger">
          <XCircle aria-hidden />
          Failed
        </Badge>
      );
    case "cancelled":
      return (
        <Badge tone="warning">
          <CircleSlash aria-hidden />
          Cancelled
        </Badge>
      );
    default:
      return <Badge>{String(status)}</Badge>;
  }
}

function duration(job: Job): string | null {
  if (!job.started_at) return null;
  const end = job.finished_at ? new Date(job.finished_at).getTime() : Date.now();
  const s = Math.max(0, Math.round((end - new Date(job.started_at).getTime()) / 1000));
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${s % 60}s`;
}

/** Live log of one job; follows the stream while the job is active. */
export function JobLog({ job, heightClass }: { job: Job; heightClass?: string }) {
  const api = useApi();
  // Decide once, at mount: a finished job's log is complete, an active one is followed.
  const [follow] = useState(() => isJobActive(job));
  const stream = useLogStream(api.jobs.logPath(job.id), follow ? { follow: 1 } : undefined);
  const statusText =
    stream.state === "connecting"
      ? "Connecting"
      : stream.state === "streaming"
        ? "Live"
        : stream.state === "error"
          ? "Disconnected"
          : "Complete";
  return (
    <div className="flex flex-col gap-2">
      <LogViewer
        buffer={stream.buffer}
        version={stream.version}
        filename={`job-${job.id}.log`}
        heightClass={heightClass ?? "h-64"}
        status={<span>{statusText}</span>}
        emptyText={isJobActive(job) ? "Waiting for output." : "This job wrote no output."}
        controls={
          stream.state === "error" || stream.state === "ended" ? (
            <Button size="sm" variant="outline" onClick={stream.restart}>
              <RotateCw />
              Reload
            </Button>
          ) : null
        }
      />
      {stream.state === "error" && <ErrorText error={stream.error} />}
    </div>
  );
}

/**
 * A job started by an action, shown where the action was taken: status,
 * timing, outcome detail, cancel, and the live log. When the job finishes,
 * the data it changed is refetched.
 */
export function InlineJob({
  job: initial,
  defaultShowLog = true,
  onDismiss,
}: {
  job: Job;
  defaultShowLog?: boolean;
  onDismiss?: () => void;
}) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const { data: job = initial, error } = useJob(initial.id, initial);
  const [showLog, setShowLog] = useState(defaultShowLog);
  const wasActive = useRef(isJobActive(initial));

  useEffect(() => {
    if (wasActive.current && !isJobActive(job)) {
      wasActive.current = false;
      invalidateJobSubject(queryClient, job);
    }
  }, [job, queryClient]);

  const cancel = useMutation({
    mutationFn: () => api.jobs.cancel(job.id),
    onSuccess: () => {
      toast.success("Cancellation requested.");
      void queryClient.invalidateQueries({ queryKey: qk.jobs.detail(job.id) });
    },
    onError: (err) => toast.error(err, "Could not cancel the job"),
  });

  const active = isJobActive(job);
  const d = duration(job);
  return (
    <div className="flex flex-col gap-2 rounded-md border border-border bg-muted/20 p-3" aria-label={`Job ${job.id}`}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm">
          <span className="font-medium">{jobKindLabel(job.kind)}</span>
          <JobStatus status={job.status} />
          <span className="text-xs text-muted-foreground">
            Job {job.id}
            {d && `, ${d}`}
          </span>
        </div>
        <div className="flex items-center gap-1.5">
          {active && (
            <Button size="sm" variant="outline" loading={cancel.isPending} onClick={() => cancel.mutate()}>
              Cancel job
            </Button>
          )}
          <Button size="sm" variant="ghost" onClick={() => setShowLog((v) => !v)} aria-expanded={showLog}>
            {showLog ? <ChevronDown /> : <ChevronRight />}
            {showLog ? "Hide log" : "Show log"}
          </Button>
          {onDismiss && !active && (
            <Button size="sm" variant="ghost" onClick={onDismiss}>
              Dismiss
            </Button>
          )}
        </div>
      </div>
      {job.detail && (
        <p className={job.status === "error" ? "text-sm text-destructive" : "text-sm text-muted-foreground"}>{job.detail}</p>
      )}
      {error && <ErrorText error={error} />}
      {showLog && <JobLog job={job} />}
    </div>
  );
}

/** Recent jobs for one project or server, each expandable to its log. */
export function JobsPanel({ subjectType, subjectId }: { subjectType: SubjectType; subjectId: number }) {
  const jobs = useSubjectJobs(subjectType, subjectId);
  const account = useAccount();
  const [open, setOpen] = useState<number | null>(null);

  if (jobs.isPending) return <LoadingState label="Loading jobs" />;
  if (jobs.isError) return <ErrorState error={jobs.error} title="Could not load jobs" onRetry={() => void jobs.refetch()} />;
  if (jobs.data.items.length === 0) return <EmptyState title="No jobs yet" description="Operations you start appear here with their logs." />;

  return (
    <ul className="flex flex-col divide-y divide-border rounded-md border border-border">
      {jobs.data.items.map((job) => {
        const expanded = open === job.id;
        const by =
          job.created_by === null
            ? "system"
            : job.created_by === account.data?.id
              ? "you"
              : `account ${job.created_by}`;
        return (
          <li key={job.id} className="flex flex-col">
            <button
              type="button"
              onClick={() => setOpen(expanded ? null : job.id)}
              aria-expanded={expanded}
              className="flex w-full flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-left text-sm hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
            >
              {expanded ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
              <span className="font-medium">{jobKindLabel(job.kind)}</span>
              <JobStatus status={job.status} />
              <span className="text-xs text-muted-foreground" title={formatDateTime(job.created_at)}>
                {formatRelative(job.created_at)} by {by}
              </span>
              {job.detail && <span className="min-w-0 truncate text-xs text-muted-foreground">{job.detail}</span>}
            </button>
            {expanded && (
              <div className="px-3 pb-3">
                <JobLog job={job} />
              </div>
            )}
          </li>
        );
      })}
    </ul>
  );
}
