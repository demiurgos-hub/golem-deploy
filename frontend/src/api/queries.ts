import { useQuery, type QueryClient } from "@tanstack/react-query";
import { useApi } from "@/app/services";
import { qk } from "@/api/query-keys";
import type { Job, Project, Server, SubjectType } from "@/api/types.gen";

export function isJobActive(job: Pick<Job, "status"> | undefined): boolean {
  return job?.status === "queued" || job?.status === "running";
}

export function isProjectBusy(p: Pick<Project, "status">): boolean {
  return p.status === "cloning" || p.status === "pulling" || p.status === "switching_branch";
}

/** Invalidates what a finished job may have changed. */
export function invalidateJobSubject(queryClient: QueryClient, job: Job) {
  const id = Number(job.subject_id);
  void queryClient.invalidateQueries({ queryKey: qk.jobs.forSubject(job.subject_type, id) });
  void queryClient.invalidateQueries({ queryKey: qk.status });
  if (job.subject_type === "project") {
    void queryClient.invalidateQueries({ queryKey: qk.projects.all });
    void queryClient.invalidateQueries({ queryKey: qk.images });
  } else if (job.subject_type === "server") {
    void queryClient.invalidateQueries({ queryKey: qk.servers.all });
    void queryClient.invalidateQueries({ queryKey: qk.volumes });
    void queryClient.invalidateQueries({ queryKey: qk.images });
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
    refetchInterval: (q) => (q.state.data?.items.some(isJobActive) ? 2000 : false),
  });
}

export function useProjects() {
  const api = useApi();
  return useQuery({
    queryKey: qk.projects.list,
    queryFn: ({ signal }) => api.projects.list(signal),
    refetchInterval: (q) => (q.state.data?.items.some(isProjectBusy) ? 3000 : false),
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

/** Server runtime state changes on its own (crashes, restarts), so lists poll while visible. */
export function useServers() {
  const api = useApi();
  return useQuery({
    queryKey: qk.servers.list,
    queryFn: ({ signal }) => api.servers.list(signal),
    refetchInterval: 5000,
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
