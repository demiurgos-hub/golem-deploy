import { useQuery, type QueryClient } from "@tanstack/react-query";
import { useApi } from "@/app/services";
import { qk } from "@/api/query-keys";
import type { Build, Job, Project, Server, SubjectType } from "@/api/types.gen";

export function isJobActive(job: Pick<Job, "status"> | undefined): boolean {
  return job?.status === "queued" || job?.status === "running";
}

/**
 * Polling of lists while the page is visible (the query client never polls
 * in the background): fast while something is in progress, slowly otherwise
 * so that changes made by other users or API tokens appear on their own.
 */
export const POLL_ACTIVE_MS = 2000;
export const POLL_IDLE_MS = 10_000;

export function listPollInterval(active: boolean): number {
  return active ? POLL_ACTIVE_MS : POLL_IDLE_MS;
}

/** A server whose state is still settling (a start, stop or crash loop in progress). */
function isServerSettling(s: Pick<Server, "state" | "desired_state">): boolean {
  return s.state === "restarting" || (s.desired_state === "running" ? s.state !== "running" : s.state === "running");
}

export function isBuildActive(b: Pick<Build, "status"> | undefined): boolean {
  return b?.status === "queued" || b?.status === "running";
}

export function isProjectBusy(p: Pick<Project, "status">): boolean {
  return p.status === "cloning" || p.status === "pulling" || p.status === "switching_branch";
}

/** Invalidates what a finished job may have changed. */
export function invalidateJobSubject(queryClient: QueryClient, job: Job) {
  void queryClient.invalidateQueries({ queryKey: qk.jobs.forSubject(job.subject_type, job.subject_id) });
  void queryClient.invalidateQueries({ queryKey: qk.status });
  if (job.subject_type === "project") {
    void queryClient.invalidateQueries({ queryKey: qk.projects.all });
    void queryClient.invalidateQueries({ queryKey: qk.builds.all });
    void queryClient.invalidateQueries({ queryKey: qk.images });
  } else if (job.subject_type === "server") {
    void queryClient.invalidateQueries({ queryKey: qk.servers.all });
    // A start records the game's schema fingerprint on the build.
    void queryClient.invalidateQueries({ queryKey: qk.builds.all });
    void queryClient.invalidateQueries({ queryKey: qk.volumes });
    void queryClient.invalidateQueries({ queryKey: qk.images });
  } else if (job.subject_type === "certificate") {
    void queryClient.invalidateQueries({ queryKey: qk.certificates.all });
    void queryClient.invalidateQueries({ queryKey: qk.servers.all });
  }
}

export function useAccount() {
  const api = useApi();
  return useQuery({ queryKey: qk.account, queryFn: ({ signal }) => api.account.get(signal), staleTime: 60_000 });
}

/** Status changes without user action (Docker reachability), so it is polled while visible. */
export function useStatus() {
  const api = useApi();
  return useQuery({
    queryKey: qk.status,
    queryFn: ({ signal }) => api.system.status(signal),
    refetchInterval: 30_000,
  });
}

export function useSettings() {
  const api = useApi();
  return useQuery({ queryKey: qk.settings, queryFn: ({ signal }) => api.system.settings(signal) });
}

export function useTokens() {
  const api = useApi();
  return useQuery({ queryKey: qk.tokens, queryFn: ({ signal }) => api.account.listTokens(signal) });
}

/** Public: whether the login page should offer registration. */
export function useAuthOptions() {
  const api = useApi();
  return useQuery({ queryKey: qk.authOptions, queryFn: ({ signal }) => api.auth.options(signal), staleTime: 30_000 });
}

export function useAccountTokens(id: number) {
  const api = useApi();
  return useQuery({ queryKey: qk.accountTokens(id), queryFn: ({ signal }) => api.accounts.listTokens(id, signal) });
}

export function useAccounts() {
  const api = useApi();
  return useQuery({ queryKey: qk.accounts, queryFn: ({ signal }) => api.accounts.list(signal) });
}

export function useJob(id: number, initial?: Job) {
  const api = useApi();
  return useQuery({
    queryKey: qk.jobs.detail(id),
    queryFn: ({ signal }) => api.jobs.get(id, signal),
    initialData: initial,
    staleTime: 0,
    refetchInterval: (q) => (isJobActive(q.state.data) ? 1500 : false),
  });
}

export function useSubjectJobs(type: SubjectType, id: number, limit = 20) {
  const api = useApi();
  return useQuery({
    queryKey: [...qk.jobs.forSubject(type, id), limit],
    queryFn: ({ signal }) => api.jobs.list({ subject_type: type, subject_id: String(id), limit }, signal),
    // Fast while a job runs; slowly otherwise, to pick up jobs started elsewhere (other users, API tokens).
    refetchInterval: (q) => listPollInterval(q.state.data?.items.some(isJobActive) ?? false),
  });
}

export function useProjects() {
  const api = useApi();
  return useQuery({
    queryKey: qk.projects.list,
    queryFn: ({ signal }) => api.projects.list(signal),
    refetchInterval: (q) => listPollInterval(q.state.data?.items.some(isProjectBusy) ?? false),
  });
}

export function useProject(id: number) {
  const api = useApi();
  return useQuery({
    queryKey: qk.projects.detail(id),
    queryFn: ({ signal }) => api.projects.get(id, signal),
    refetchInterval: (q) => (q.state.data && isProjectBusy(q.state.data) ? 2000 : false),
  });
}

export function useBranches(id: number, enabled: boolean) {
  const api = useApi();
  return useQuery({
    queryKey: qk.projects.branches(id),
    queryFn: ({ signal }) => api.projects.branches(id, signal),
    enabled,
  });
}

/** Server runtime state changes on its own (crashes, restarts) and through other users, so the list polls while visible. */
export function useServers() {
  const api = useApi();
  return useQuery({
    queryKey: qk.servers.list,
    queryFn: ({ signal }) => api.servers.list(signal),
    refetchInterval: (q) => listPollInterval(q.state.data?.items.some(isServerSettling) ?? false),
  });
}

export function useServer(id: number) {
  const api = useApi();
  return useQuery({
    queryKey: qk.servers.detail(id),
    queryFn: ({ signal }) => api.servers.get(id, signal),
    refetchInterval: 4000,
  });
}

export function useServerStats(server: Server | undefined) {
  const api = useApi();
  const running = server?.state === "running";
  return useQuery({
    queryKey: qk.servers.stats(server?.id ?? 0),
    queryFn: ({ signal }) => api.servers.stats(server?.id ?? 0, signal),
    enabled: running,
    staleTime: 0,
    refetchInterval: running ? 3000 : false,
  });
}

/** The project's builds, newest first; polled fast while one is queued or running, slowly otherwise. */
export function useProjectBuilds(projectId: number) {
  const api = useApi();
  return useQuery({
    queryKey: qk.builds.forProject(projectId),
    queryFn: ({ signal }) => api.projects.builds(projectId, { limit: 100 }, signal),
    refetchInterval: (q) => listPollInterval(q.state.data?.items.some(isBuildActive) ?? false),
  });
}

/** Successful builds of every project, newest first: what a server can run. */
export function useSuccessfulBuilds(enabled = true) {
  const api = useApi();
  return useQuery({
    queryKey: qk.builds.successful,
    queryFn: ({ signal }) => api.builds.list({ status: "success", limit: 500 }, signal),
    enabled,
  });
}

export function useBuild(id: number) {
  const api = useApi();
  return useQuery({
    queryKey: qk.builds.detail(id),
    queryFn: ({ signal }) => api.builds.get(id, signal),
    // Active builds change on their own; a finished one only gains a schema fingerprint or servers.
    refetchInterval: (q) => (isBuildActive(q.state.data) ? 1500 : POLL_IDLE_MS),
  });
}

/**
 * The running game's runtime view, sampled while the server runs and the page
 * is visible. 409 (not running) and 503 (admin endpoint silent, Docker
 * unreachable) are expected answers, not failures to retry.
 */
export function useServerRuntime(server: Server | undefined) {
  const api = useApi();
  const running = server?.state === "running";
  return useQuery({
    queryKey: qk.servers.runtime(server?.id ?? 0),
    queryFn: ({ signal }) => api.servers.runtime(server?.id ?? 0, signal),
    enabled: running,
    staleTime: 0,
    retry: false,
    refetchInterval: running ? 3000 : false,
  });
}

export function useGameCertificate() {
  const api = useApi();
  return useQuery({
    queryKey: qk.certificates.game,
    queryFn: ({ signal }) => api.certificates.game(signal),
    refetchInterval: 30_000,
    retry: false,
  });
}

export function usePanelCertificate() {
  const api = useApi();
  return useQuery({
    queryKey: qk.certificates.panel,
    queryFn: ({ signal }) => api.certificates.panel(signal),
    refetchInterval: 30_000,
    retry: false,
  });
}

export function useACME() {
  const api = useApi();
  return useQuery({
    queryKey: qk.certificates.acme,
    queryFn: ({ signal }) => api.certificates.acme(signal),
    refetchInterval: 30_000,
    retry: false,
  });
}

export function useImages() {
  const api = useApi();
  return useQuery({ queryKey: qk.images, queryFn: ({ signal }) => api.images.list(signal) });
}

export function useStaticFiles() {
  const api = useApi();
  return useQuery({ queryKey: qk.staticFiles, queryFn: ({ signal }) => api.staticFiles.list(signal) });
}

export function useVolumes() {
  const api = useApi();
  return useQuery({ queryKey: qk.volumes, queryFn: ({ signal }) => api.volumes.list(signal) });
}
