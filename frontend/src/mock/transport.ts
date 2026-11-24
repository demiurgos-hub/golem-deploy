/**
 * Dev-only stand-in for the Docker- and git-backed routes (projects,
 * servers, images, static files, volumes, and their jobs), so the web
 * client can be developed without Docker, git or network access. It follows
 * the final API v1 contract (docs/API.md) and the generated wire types:
 * access levels, restricted jobs, error codes and messages, the follow
 * stream cap, tail bounds, static replace semantics and import validation.
 * Everything else (auth, account, admin, settings, status, other jobs) and
 * the caller's identity come from the real daemon.
 *
 * Enabled with VITE_API_MOCK=1 under `vite` (dev). It is imported only behind
 * `import.meta.env.DEV`, so production builds drop it; vite.config.ts fails
 * a production build that has the flag set or that contains MOCK_MARKER.
 */
import type { RawResponse, Transport, UploadRequest } from "@/api/client";
import type {
  CreateProjectRequest,
  CreateServerRequest,
  CurrentAccount,
  ErrorCode,
  Image,
  Job,
  JobKind,
  Project,
  ProjectAuth,
  Server,
  ServerExport,
  StaticFile,
  SubjectType,
  SwitchBranchRequest,
  UpdateProjectRequest,
  UpdateServerRequest,
  Volume,
} from "@/api/types.gen";
import { checkRepoUrl, sameRepoOrigin, validateEnvKey, validateEnvTotals, validateEnvValue, validateStaticName } from "@/lib/validators";

export const MOCK_MARKER = "golem-deploy-mock-layer";

const BASE = "/api/v1";
const NAME_RE = /^[a-z0-9][a-z0-9-]{0,62}$/;
const PORT_RANGE: [number, number] = [7000, 7099];
const FOLLOW_CAP = 4;
const FLOOD_LINES = 50_000;
const MAX_TAIL = 5000;
const STATIC_QUOTA = 200 * 1024 * 1024;
const STATIC_MAX_FILES = 2000;
const MAX_UPLOADS = 2;
const MAX_IMPORT = 128;
const MAX_IMAGE_VOLUMES = 8;

interface MockState {
  projects: Project[];
  servers: Server[];
  env: Map<number, Record<string, string>>;
  images: Image[];
  statics: Map<string, { meta: StaticFile; blob: Blob }>;
  volumes: Volume[];
  jobs: Job[];
  jobLogs: Map<number, string[]>;
  follows: Map<number, number>;
  uploads: Map<number, number>;
  nextId: number;
  nextJobId: number;
  requestSeq: number;
}

type Me = Pick<CurrentAccount, "id" | "username" | "role">;

const iso = (msAgo = 0) => new Date(Date.now() - msAgo).toISOString();
const HOUR = 3_600_000;
const DAY = 24 * HOUR;
const IMG_ARENA = "sha256:4f1c2a9e0b7d3c5a8e6f1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e";
const IMG_ARENA_OLD = "sha256:1111111111111111111111111111111111111111111111111111111111111111";
const IMG_KART = "sha256:9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9a8b7c6d5e4f3a2b1c0d9e8f";
// Declares more than 8 VOLUMEs: the daemon refuses to run it.
const IMG_VOLUMES = "sha256:5555555555555555555555555555555555555555555555555555555555555555";
const VOLUME_COUNT: Record<string, number> = { [IMG_VOLUMES]: 12 };

function seed(): MockState {
  const project = (id: number, name: string, repo: string, extra: Partial<Project>): Project => ({
    id,
    name,
    repo_url: repo,
    branch: "main",
    auth_type: "none",
    auth_username: "",
    has_secret: false,
    last_commit: "4f1c2a9e0b7d3c5a8e6f1b2c3d4e5f6a7b8c9d0e",
    last_pulled_at: iso(2 * HOUR),
    status: "idle",
    created_at: iso(20 * DAY),
    updated_at: iso(2 * HOUR),
    ...extra,
  });
  const projects: Project[] = [
    project(1, "arena-shooter", "https://github.com/example/arena-shooter.git", {}),
    project(2, "kart-racer", "git@gitlab.example.com:studio/kart-racer.git", {
      branch: "develop",
      auth_type: "ssh",
      has_secret: true,
      last_commit: "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b",
      last_pulled_at: iso(3 * DAY),
    }),
    // A failed clone: no checkout, repaired by pulling.
    project(3, "private-sandbox", "https://git.example.com/team/sandbox.git", {
      auth_type: "https",
      auth_username: "deploy-bot",
      has_secret: true,
      last_commit: "",
      last_pulled_at: null,
      status: "error",
    }),
  ];
  const server = (id: number, name: string, port: number, desired: Server["desired_state"], state: Server["state"], extra: Partial<Server> = {}): Server => ({
    id,
    name,
    project_id: 1,
    image: IMG_ARENA,
    port,
    env_keys: ["ADMIN_TOKEN", "MAX_PLAYERS"],
    desired_state: desired,
    state,
    container_id: state === "missing" ? "" : `c0ffee${id}a1b2c3d4e5f60718293a4b5c6d7e8f90`,
    started_at: state === "running" ? iso(5 * HOUR) : null,
    exit_code: state === "exited" ? 0 : null,
    restart_required: false,
    created_at: iso(10 * DAY),
    updated_at: iso(6 * HOUR),
    ...extra,
  });
  const servers: Server[] = [
    server(11, "eu-arena", 7000, "running", "running"),
    server(12, "staging", 7001, "stopped", "exited"),
    server(13, "crashy", 7002, "running", "exited", { exit_code: 137, updated_at: iso(HOUR) }),
    server(14, "flood-test", 7003, "running", "running", { env_keys: [] }),
    server(15, "kart-eu", 7004, "running", "running", { project_id: 2, image: IMG_KART, restart_required: true }),
    server(16, "fresh", 7005, "stopped", "missing", { env_keys: [] }),
    // Has a container, but "Docker" cannot be queried for it: every action fails, delete answers 503.
    server(17, "docker-lost", 7006, "running", "unknown", { env_keys: [], started_at: null }),
  ];
  const env = new Map<number, Record<string, string>>();
  for (const s of servers) env.set(s.id, Object.fromEntries(s.env_keys.map((k) => [k, "secret"])));
  const image = (id: string, tags: string[], projectId: number | null, commit: string, ageMs: number, size: number): Image => ({
    id,
    tags,
    project_id: projectId,
    commit,
    size_bytes: size,
    created_at: iso(ageMs),
    server_ids: [],
    in_use: false,
  });
  const images: Image[] = [
    image(IMG_ARENA, ["golem/arena-shooter:4f1c2a9", "golem/arena-shooter:latest"], 1, "4f1c2a9e0b7d3c5a8e6f1b2c3d4e5f6a7b8c9d0e", 2 * HOUR, 187_400_000),
    image(IMG_ARENA_OLD, ["golem/arena-shooter:0d3e2f1"], 1, "0d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e", 9 * DAY, 185_100_000),
    image(IMG_KART, ["golem/kart-racer:9a8b7c6"], 2, "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b", 3 * DAY, 243_900_000),
    image(IMG_VOLUMES, ["golem/many-volumes:latest"], null, "", 4 * DAY, 12_000_000),
  ];
  const statics = new Map<string, { meta: StaticFile; blob: Blob }>();
  for (const [name, text, size] of [
    ["maps-pack-v3.zip", "PK\u0003\u0004pretend zip", 48_300_000],
    ["sound_bank.bin", "pretend audio", 9_100_000],
  ] as const) {
    statics.set(name, { meta: { name, size_bytes: size, modified_at: iso(5 * DAY), is_zip: name.endsWith(".zip") }, blob: new Blob([text]) });
  }
  // Servers have no data volumes yet (a later milestone), so the list starts empty like on a real host.
  return { projects, servers, env, images, statics, volumes: [], jobs: [], jobLogs: new Map(), follows: new Map(), uploads: new Map(), nextId: 100, nextJobId: 900_001, requestSeq: 0 };
}

function headers(st: MockState, extra: Record<string, string> = {}): Headers {
  st.requestSeq++;
  return new Headers({ "X-Request-Id": `mock-${st.requestSeq.toString(16).padStart(6, "0")}`, ...extra });
}

function json(st: MockState, status: number, body: unknown): Response {
  if (status === 204) return new Response(null, { status, headers: headers(st) });
  return new Response(JSON.stringify(body), { status, headers: headers(st, { "Content-Type": "application/json" }) });
}

function errorBody(st: MockState, code: ErrorCode, message: string, field?: string, extra: Record<string, string> = {}) {
  const h = headers(st, { "Content-Type": "application/json", ...extra });
  return { h, text: JSON.stringify({ error: { code, message, request_id: h.get("X-Request-Id"), ...(field ? { field } : {}) } }) };
}

function fail(st: MockState, status: number, code: ErrorCode, message: string, field?: string, extra: Record<string, string> = {}): Response {
  const { h, text } = errorBody(st, code, message, field, extra);
  return new Response(text, { status, headers: h });
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const isActive = (j: Job) => j.status === "queued" || j.status === "running";

function activeJobFor(st: MockState, type: SubjectType, id: number): Job | undefined {
  return st.jobs.find((j) => j.subject_type === type && j.subject_id === String(id) && isActive(j));
}

/** Starts a fake job that logs a few lines per step and then runs `finish`. A step "FAIL" makes it fail with `failDetail`. */
function startJob(
  st: MockState,
  me: Me,
  restricted: boolean,
  kind: JobKind,
  type: SubjectType,
  id: number,
  steps: string[],
  finish: (ok: boolean) => void,
  failDetail = "authentication with the Git remote failed; check the project's credentials",
): Job {
  const job: Job = {
    id: st.nextJobId++,
    kind,
    subject_type: type,
    subject_id: String(id),
    status: "queued",
    detail: "",
    restricted,
    created_by: me.id,
    created_by_username: me.username,
    created_at: iso(),
    started_at: null,
    finished_at: null,
  };
  st.jobs.unshift(job);
  const log: string[] = [];
  st.jobLogs.set(job.id, log);
  const failing = steps.includes("FAIL");
  let i = 0;
  setTimeout(() => {
    if (job.status !== "queued") return;
    job.status = "running";
    job.started_at = iso();
    const tick = setInterval(() => {
      if (job.status !== "running") {
        clearInterval(tick);
        return;
      }
      if (i < steps.length) {
        const step = steps[i++];
        if (step.startsWith("container | ")) log.push(step);
        else if (step !== "FAIL") for (let n = 0; n < 4; n++) log.push(`${step}${n ? ` (${n}/3)` : ""}`);
        return;
      }
      clearInterval(tick);
      job.status = failing ? "error" : "success";
      job.detail = failing ? failDetail : "";
      job.finished_at = iso();
      log.push(failing ? `error: ${failDetail}` : "done");
      finish(!failing);
    }, 400);
  }, 300);
  return job;
}

function streamLines(produce: (push: (lines: string[]) => void, close: () => void) => () => void, onEnd: () => void): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  let stop: () => void = () => {};
  let ended = false;
  const end = () => {
    if (!ended) {
      ended = true;
      onEnd();
    }
  };
  return new ReadableStream<Uint8Array>({
    start(controller) {
      let closed = false;
      stop = produce(
        (lines) => {
          if (!closed && lines.length) controller.enqueue(enc.encode(lines.join("\n") + "\n"));
        },
        () => {
          if (!closed) {
            closed = true;
            controller.close();
            end();
          }
        },
      );
    },
    cancel() {
      stop();
      end();
    },
  });
}

/** User-safe job details the daemon gives for git failures, picked by a word in the URL. */
function gitFailDetail(url: string): string {
  if (url.includes("toolarge")) return "the checkout exceeds the maximum repository size configured on the host (--max-repo-size-mb); the previous checkout was kept";
  if (url.includes("lowdisk")) return "less than 512 MiB is free on the data directory's file system; free some disk space and try again";
  if (url.includes("lfsconfig")) return "the repository's .lfsconfig sets the Git LFS server or transfer settings, which is not allowed; the job log names the keys";
  return "authentication with the Git remote failed; check the project's credentials";
}

const failsGit = (url: string) => /fail|toolarge|lowdisk|lfsconfig/.test(url);

/** A StaticFileList: `items` plus the usage of the whole store. */
function staticList(st: MockState, items: StaticFile[]) {
  const used = [...st.statics.values()].reduce((n, f) => n + f.meta.size_bytes, 0);
  return { items, used_bytes: used, quota_bytes: STATIC_QUOTA, file_count: st.statics.size, max_files: STATIC_MAX_FILES };
}

function nextPort(st: MockState): number | null {
  for (let p = PORT_RANGE[0]; p <= PORT_RANGE[1]; p++) if (!st.servers.some((s) => s.port === p)) return p;
  return null;
}

/** Image id or tag of a managed image, resolved to its id (like the daemon). */
function resolveImage(st: MockState, ref: string): string | null {
  const r = ref.trim();
  if (!r) return null;
  const img = st.images.find((i) => i.id === r || i.tags.includes(r) || (r.length >= 12 && i.id.replace(/^sha256:/, "").startsWith(r.replace(/^sha256:/, ""))));
  return img ? img.id : null;
}

const IMAGE_MESSAGE = "image must be a managed image (built and labelled by golem-deploy)";
const VOLUMES_MESSAGE = `the image declares more than ${MAX_IMAGE_VOLUMES} volumes`;
const tooManyVolumes = (id: string) => (VOLUME_COUNT[id] ?? 0) > MAX_IMAGE_VOLUMES;

/** Like the daemon's validateEnv: names and limits name `field`, a rejected value `field.KEY`. */
function envError(env: Record<string, string> | undefined, field: string): { field: string; message: string } | null {
  const seen = new Set<string>();
  const all = env ?? {};
  const totals = validateEnvTotals(all);
  if (Object.keys(all).length > 128 && totals) return { field, message: "at most 128 variables are allowed" };
  for (const k of Object.keys(all).sort()) {
    if (!/^[A-Za-z_][A-Za-z0-9_]{0,127}$/.test(k)) return { field, message: "variable names must match ^[A-Za-z_][A-Za-z0-9_]{0,127}$" };
    if (validateEnvKey(k)) return { field, message: `variable names starting with GOLEM_ are reserved for golem-deploy ("${k}")` };
    if (seen.has(k.toUpperCase())) return { field, message: `variable names must be unique ignoring case ("${k}")` };
    seen.add(k.toUpperCase());
    const ve = validateEnvValue(k, all[k]);
    if (ve) {
      const message = ve.includes("line break")
        ? "the value contains a line break (values must be a single line)"
        : ve.includes("NUL")
          ? "the value contains a NUL byte"
          : "the variable exceeds 32768 bytes";
      return { field: `${field}.${k}`, message };
    }
  }
  if (totals) return { field, message: "variables exceed 65536 bytes in total" };
  return null;
}

function portError(port: number): string | null {
  return port < PORT_RANGE[0] || port > PORT_RANGE[1] ? `port must be within the configured range ${PORT_RANGE[0]}-${PORT_RANGE[1]}` : null;
}

function authError(auth: ProjectAuth | undefined, kind: "https" | "ssh"): { field: string; message: string } | null {
  if (!auth || !auth.type) return { field: "auth.type", message: 'auth.type is required ("none", "https" or "ssh")' };
  if (auth.type === "none") {
    if (kind === "ssh") return { field: "auth.type", message: 'SSH repository URLs need a private key: use auth type "ssh" with a deploy key' };
  } else if (auth.type === "https") {
    if (kind !== "https") return { field: "auth.type", message: "https credentials require an https:// repository URL" };
    if (!auth.username) return { field: "auth.username", message: "username is required for https authentication" };
    if (!auth.password) return { field: "auth.password", message: "password is required for https authentication" };
  } else if (auth.type === "ssh") {
    if (kind !== "ssh") return { field: "auth.type", message: "an SSH key requires an ssh:// or user@host:path repository URL" };
    if (!auth.private_key?.includes("PRIVATE KEY")) {
      return { field: "auth.private_key", message: "the SSH private key could not be parsed; provide an unencrypted OpenSSH or PEM private key" };
    }
  }
  return null;
}

export function createMockTransport(real: Transport): Transport {
  const st = seed();
  const identities = new Map<string, Me>();
  console.info(`${MOCK_MARKER}: VITE_API_MOCK=1, projects/servers/images/static/volumes are simulated in the browser.`);

  /** The caller, as the real daemon sees the bearer token; null lets the request through to the daemon (which answers 401). */
  async function identify(init: RequestInit | { headers: Record<string, string> }): Promise<Me | null> {
    const auth = (init.headers as Record<string, string> | undefined)?.Authorization;
    if (!auth) return null;
    const cached = identities.get(auth);
    if (cached) return cached;
    const res = await real.fetch(`${BASE}/account`, { headers: { Authorization: auth, Accept: "application/json" }, credentials: "omit" });
    if (!res.ok) return null;
    const me = (await res.json()) as CurrentAccount;
    const out: Me = { id: me.id, username: me.username, role: me.auth === "api_token" ? "developer" : me.role };
    identities.set(auth, out);
    return out;
  }

  const forbidden = () => fail(st, 403, "forbidden", "your role does not allow this action");

  function refreshImages() {
    for (const img of st.images) {
      img.server_ids = st.servers.filter((s) => s.image === img.id).map((s) => s.id);
      img.in_use = img.server_ids.length > 0;
    }
  }

  function jobLogStream(job: Job, follow: boolean, onEnd: () => void): ReadableStream<Uint8Array> {
    return streamLines((push, close) => {
      const log = st.jobLogs.get(job.id) ?? [];
      let sent = 0;
      const drain = () => {
        push(log.slice(sent));
        sent = log.length;
      };
      drain();
      if (!follow || !isActive(job)) {
        close();
        return () => {};
      }
      const t = setInterval(() => {
        drain();
        if (!isActive(job)) {
          clearInterval(t);
          close();
        }
      }, 250);
      return () => clearInterval(t);
    }, onEnd);
  }

  function serverLogStream(server: Server, tail: number, follow: boolean, onEnd: () => void): ReadableStream<Uint8Array> {
    const line = (n: number) =>
      `${new Date().toISOString()} [${n % 17 === 0 ? "WARN" : "INFO"}] ${server.name}: tick ${n} players=${(n * 7) % 33} rtt=${(n * 13) % 90}ms` +
      // The daemon escapes control characters (ESC arrives as the text \x1b) and bidi overrides (\u202e).
      (n % 101 === 0 ? " \\x1b[31mcolour codes arrive escaped\\x1b[0m, a bidi override \\u202e too; snowman ☃ éè" : "");
    return streamLines((push, close) => {
      let n = 0;
      const timers: ReturnType<typeof setInterval>[] = [];
      const running = server.state === "running";
      if (server.name === "flood-test" && running && follow) {
        // 50k lines as fast as possible, to exercise windowing and the buffer cap.
        const t = setInterval(() => {
          const batch: string[] = [];
          for (let i = 0; i < 2000 && n < FLOOD_LINES; i++) batch.push(line(n++));
          push(batch);
          if (n >= FLOOD_LINES) clearInterval(t);
        }, 15);
        timers.push(t);
      } else {
        const history: string[] = [];
        for (let i = 0; i < Math.min(tail, 400); i++) history.push(line(n++));
        push(history);
        // A stopped container still has its last output; follow ends right after it.
        if (!follow || !running) {
          close();
          return () => {};
        }
      }
      timers.push(
        setInterval(() => {
          if (server.state !== "running" || !st.servers.includes(server)) {
            timers.forEach(clearInterval);
            close();
            return;
          }
          push([line(n++)]);
        }, 700),
      );
      return () => timers.forEach(clearInterval);
    }, onEnd);
  }

  /** Reserves one of the account's follow streams, or answers 429 like the daemon. */
  function acquireFollow(me: Me): (() => void) | Response {
    const open = st.follows.get(me.id) ?? 0;
    if (open >= FOLLOW_CAP) return fail(st, 429, "rate_limited", "too many open log streams; close one and retry", undefined, { "Retry-After": "5" });
    st.follows.set(me.id, open + 1);
    return () => st.follows.set(me.id, Math.max(0, (st.follows.get(me.id) ?? 1) - 1));
  }

  function textStream(body: ReadableStream<Uint8Array>): Response {
    return new Response(body, { status: 200, headers: headers(st, { "Content-Type": "text/plain; charset=utf-8" }) });
  }

  function parseFollow(query: URLSearchParams): boolean | null {
    const v = query.get("follow");
    if (v === null || v === "" || v === "0" || v === "false") return false;
    if (v === "1" || v === "true") return true;
    return null;
  }

  async function handle(method: string, path: string, query: URLSearchParams, init: RequestInit): Promise<Response | null> {
    const body = <T,>(): T => JSON.parse(typeof init.body === "string" ? init.body : "{}") as T;
    let m: RegExpMatchArray | null;
    const mocked =
      /^\/(projects|servers|images|static|volumes)(\/|$)/.test(path) ||
      (path === "/jobs" && (query.get("subject_type") === "project" || query.get("subject_type") === "server")) ||
      (/^\/jobs\/\d+(\/log|\/cancel)?$/.test(path) && st.jobs.some((j) => path.split("/")[2] === String(j.id)));
    if (!mocked) return null;
    const me = await identify(init);
    if (!me) return null;
    const admin = me.role === "admin";

    // Jobs of mocked subjects (mock job ids start at 900001).
    if (method === "GET" && path === "/jobs") {
      const sid = query.get("subject_id");
      const limit = Number(query.get("limit") ?? 50);
      if (!Number.isInteger(limit) || limit < 1 || limit > 500) return fail(st, 400, "bad_request", "limit must be an integer from 1 to 500");
      return json(st, 200, { items: st.jobs.filter((j) => j.subject_type === query.get("subject_type") && (!sid || j.subject_id === sid)).slice(0, limit) });
    }
    if ((m = path.match(/^\/jobs\/(\d+)(\/log|\/cancel)?$/))) {
      const job = st.jobs.find((j) => j.id === Number(m?.[1]));
      if (!job) return fail(st, 404, "not_found", "job not found");
      if (method === "GET" && !m[2]) return json(st, 200, job);
      const restrictedMsg = "this job was started by an administrator action; only admins can read its log or cancel it";
      if (method === "POST" && m[2] === "/cancel") {
        if (!admin && job.restricted) return fail(st, 403, "forbidden", restrictedMsg);
        if (!admin && job.created_by !== me.id) return fail(st, 403, "forbidden", "only the account that started this job or an admin can cancel it");
        if (!isActive(job)) return fail(st, 409, "job_not_active", "job has already finished");
        job.status = "cancelled";
        job.detail = "cancelled";
        job.finished_at = iso();
        return json(st, 202, job);
      }
      if (method === "GET" && m[2] === "/log") {
        if (!admin && job.restricted) return fail(st, 403, "forbidden", restrictedMsg);
        const follow = parseFollow(query);
        if (follow === null) return fail(st, 400, "bad_request", "follow must be 1 or 0");
        let release = () => {};
        if (follow) {
          const r = acquireFollow(me);
          if (r instanceof Response) return r;
          release = r;
        }
        return textStream(jobLogStream(job, follow, release));
      }
      return null;
    }

    // Projects.
    if (path === "/projects" && method === "GET") return json(st, 200, { items: [...st.projects].sort((a, b) => a.name.localeCompare(b.name)) });
    if (path === "/projects" && method === "POST") {
      if (!admin) return forbidden();
      const req = body<CreateProjectRequest>();
      if (!NAME_RE.test(req.name ?? "")) return fail(st, 400, "validation_failed", "name must be 1-63 characters of a-z, 0-9 and '-', starting with a letter or digit", "name");
      const url = checkRepoUrl(req.repo_url ?? "");
      if (url.error !== null) return fail(st, 400, "validation_failed", url.error, "repo_url");
      const ae = authError(req.auth, url.kind);
      if (ae) return fail(st, 400, "validation_failed", ae.message, ae.field);
      if (st.projects.some((p) => p.name === req.name)) return fail(st, 409, "name_taken", "a project with that name already exists");
      const p: Project = {
        id: st.nextId++,
        name: req.name,
        repo_url: req.repo_url,
        branch: req.branch ?? "",
        auth_type: req.auth.type,
        auth_username: req.auth.username ?? "",
        has_secret: req.auth.type !== "none",
        last_commit: "",
        last_pulled_at: null,
        status: "cloning",
        created_at: iso(),
        updated_at: iso(),
      };
      st.projects.push(p);
      const steps = ["Cloning into repository", "Receiving objects", "Resolving deltas", "Checking out files"];
      if (failsGit(req.repo_url)) steps.push("FAIL");
      // Started from an admin route: restricted.
      const job = startJob(st, me, true, "project.clone", "project", p.id, steps, (ok) => {
        p.status = ok ? "idle" : "error";
        if (ok) {
          p.branch = p.branch || "main";
          p.last_commit = "0123456789abcdef0123456789abcdef01234567";
          p.last_pulled_at = iso();
        }
      }, gitFailDetail(req.repo_url));
      return json(st, 201, { project: p, job });
    }
    if ((m = path.match(/^\/projects\/(\d+)(\/pull|\/branch|\/branches)?$/))) {
      const p = st.projects.find((x) => x.id === Number(m?.[1]));
      if (!p) return fail(st, 404, "not_found", "project not found");
      const sub = m[2];
      if (!sub && method === "GET") return json(st, 200, p);
      if (!sub && method === "PATCH") {
        if (!admin) return forbidden();
        const req = body<UpdateProjectRequest>();
        const url = checkRepoUrl(req.repo_url ?? p.repo_url);
        if (url.error !== null) return fail(st, 400, "validation_failed", url.error, "repo_url");
        if (req.auth) {
          const ae = authError(req.auth, url.kind);
          if (ae) return fail(st, 400, "validation_failed", ae.message, ae.field);
        } else if (req.repo_url !== undefined && !sameRepoOrigin(p.repo_url, req.repo_url)) {
          return fail(
            st,
            400,
            "validation_failed",
            'the new repository URL has a different scheme, host or port, and the stored credentials were set for the previous one; send auth in the same request (new credentials, or {"type":"none"} for a public https repository)',
            "auth",
          );
        } else if (p.auth_type !== "none" && p.auth_type !== url.kind) {
          return fail(st, 400, "validation_failed", `${p.auth_type === "https" ? "https credentials require an https:// repository URL" : "an SSH key requires an ssh:// or user@host:path repository URL"}; send new auth together with the URL`, "repo_url");
        }
        if (req.repo_url !== undefined) p.repo_url = req.repo_url;
        if (req.auth) {
          p.auth_type = req.auth.type;
          p.auth_username = req.auth.username ?? "";
          p.has_secret = req.auth.type !== "none";
        }
        p.updated_at = iso();
        return json(st, 200, p);
      }
      if (!sub && method === "DELETE") {
        if (!admin) return forbidden();
        if (activeJobFor(st, "project", p.id)) return fail(st, 409, "job_in_progress", "another job is already running for this project");
        st.projects = st.projects.filter((x) => x.id !== p.id);
        for (const s of st.servers) if (s.project_id === p.id) s.project_id = null;
        return json(st, 204, null);
      }
      if (sub === "/branches" && method === "GET") {
        if (p.repo_url.includes("unreachable")) return fail(st, 503, "service_unavailable", "the Git remote could not be reached");
        return json(st, 200, { current: p.branch, items: ["develop", "feature/netcode", "main"] });
      }
      if ((sub === "/pull" || sub === "/branch") && method === "POST") {
        let branch = p.branch;
        if (sub === "/branch") {
          branch = body<SwitchBranchRequest>().branch ?? "";
          if (!branch) return fail(st, 400, "validation_failed", "branch is required", "branch");
        }
        if (activeJobFor(st, "project", p.id)) return fail(st, 409, "job_in_progress", "another job is already running for this project");
        // Without a checkout (failed clone) both re-clone.
        const clone = p.last_commit === "";
        const prev = p.status;
        p.status = clone ? "cloning" : sub === "/pull" ? "pulling" : "switching_branch";
        const kind: JobKind = clone ? "project.clone" : sub === "/pull" ? "project.pull" : "project.branch";
        const steps = clone ? ["Cloning into repository", "Checking out files"] : sub === "/pull" ? ["Fetching origin", "Resetting to origin"] : [`Fetching ${branch}`, `Switched to branch '${branch}'`];
        if (failsGit(p.repo_url)) steps.push("FAIL");
        return json(
          st,
          202,
          startJob(st, me, false, kind, "project", p.id, steps, (ok) => {
            // A failure before the tree is touched leaves the status as it was.
            p.status = ok ? "idle" : clone ? "error" : prev;
            if (!ok) return;
            p.branch = branch || "main";
            p.last_pulled_at = iso();
            p.last_commit = Math.random().toString(16).slice(2).padEnd(40, "0").slice(0, 40);
          }, gitFailDetail(p.repo_url)),
        );
      }
      return null;
    }

    // Servers.
    if (path === "/servers" && method === "GET") return json(st, 200, { items: st.servers });
    if (path === "/servers" && method === "POST") {
      const req = body<CreateServerRequest>();
      if (!NAME_RE.test(req.name ?? "")) return fail(st, 400, "validation_failed", "name must be 1-63 characters of a-z, 0-9 and '-', starting with a letter or digit", "name");
      const image = resolveImage(st, req.image ?? "");
      if (!image) return fail(st, 400, "validation_failed", IMAGE_MESSAGE, "image");
      if (tooManyVolumes(image)) return fail(st, 400, "validation_failed", VOLUMES_MESSAGE, "image");
      if (req.project_id !== undefined && !st.projects.some((p) => p.id === req.project_id)) return fail(st, 400, "validation_failed", "project does not exist", "project_id");
      if (req.port !== undefined) {
        const pe = portError(req.port);
        if (pe) return fail(st, 400, "validation_failed", pe, "port");
      }
      const ee = envError(req.env, "env");
      if (ee) return fail(st, 400, "validation_failed", ee.message, ee.field);
      if (st.servers.some((s) => s.name === req.name)) return fail(st, 409, "name_taken", "a server with that name already exists");
      const port = req.port ?? nextPort(st);
      if (port === null || st.servers.some((s) => s.port === port)) return fail(st, 409, "port_in_use", "that port is already assigned to another server (or the port range is full)");
      const s: Server = {
        id: st.nextId++,
        name: req.name,
        project_id: req.project_id ?? null,
        image,
        port,
        env_keys: Object.keys(req.env ?? {}).sort(),
        desired_state: req.desired_state ?? "stopped",
        state: "missing",
        container_id: "",
        started_at: null,
        exit_code: null,
        restart_required: false,
        created_at: iso(),
        updated_at: iso(),
      };
      st.servers.push(s);
      st.env.set(s.id, { ...(req.env ?? {}) });
      if (s.desired_state === "running") startServerAction(s, "start", me);
      return json(st, 201, s);
    }
    if (path === "/servers/export" && method === "GET") {
      if (!admin) return forbidden();
      const out: ServerExport = {
        format_version: 1,
        servers: st.servers.map((s) => ({
          name: s.name,
          ...(s.project_id !== null ? { project: st.projects.find((p) => p.id === s.project_id)?.name } : {}),
          image: s.image,
          port: s.port,
          env: st.env.get(s.id) ?? {},
          desired_state: s.desired_state,
        })),
      };
      return json(st, 200, out);
    }
    if (path === "/servers/import" && method === "POST") {
      if (!admin) return forbidden();
      const req = body<ServerExport>();
      if (req.format_version !== 1) return fail(st, 400, "validation_failed", "format_version must be 1", "format_version");
      if (!req.servers?.length) return fail(st, 400, "validation_failed", "the document contains no servers", "servers");
      if (req.servers.length > MAX_IMPORT) return fail(st, 400, "validation_failed", `at most ${MAX_IMPORT} servers can be imported at once`, "servers");
      // Like the daemon: stop at the first problem, one error envelope, nothing created.
      const names = new Set<string>();
      const ports = new Set<number>();
      const resolved: { image: string; projectId: number | null }[] = [];
      for (const [i, spec] of req.servers.entries()) {
        const f = (n: string) => `servers[${i}].${n}`;
        if (!NAME_RE.test(spec.name ?? "")) return fail(st, 400, "validation_failed", "name must be 1-63 characters of a-z, 0-9 and '-', starting with a letter or digit", f("name"));
        if (names.has(spec.name)) return fail(st, 400, "validation_failed", `duplicate server name "${spec.name}" in the document`, f("name"));
        names.add(spec.name);
        if (st.servers.some((s) => s.name === spec.name)) return fail(st, 409, "name_taken", "a server with that name already exists");
        if (spec.desired_state && spec.desired_state !== "running" && spec.desired_state !== "stopped") return fail(st, 400, "validation_failed", 'desired_state must be "running" or "stopped"', f("desired_state"));
        const ee = envError(spec.env, f("env"));
        if (ee) return fail(st, 400, "validation_failed", ee.message, ee.field);
        let projectId: number | null = null;
        if (spec.project) {
          const p = st.projects.find((x) => x.name === spec.project?.toLowerCase());
          if (!p) return fail(st, 400, "validation_failed", `project "${spec.project}" does not exist`, f("project"));
          projectId = p.id;
        }
        if (spec.port !== undefined) {
          const pe = portError(spec.port);
          if (pe) return fail(st, 400, "validation_failed", pe, f("port"));
          if (ports.has(spec.port)) return fail(st, 400, "validation_failed", `duplicate port ${spec.port} in the document`, f("port"));
          if (st.servers.some((s) => s.port === spec.port)) return fail(st, 409, "port_in_use", "that port is already assigned to another server (or the port range is full)");
          ports.add(spec.port);
        }
        const image = resolveImage(st, spec.image ?? "");
        if (!image) return fail(st, 400, "validation_failed", IMAGE_MESSAGE, f("image"));
        if (tooManyVolumes(image)) return fail(st, 400, "validation_failed", VOLUMES_MESSAGE, f("image"));
        resolved.push({ image, projectId });
      }
      const created = req.servers.map((spec, i) => {
        const port = spec.port ?? nextPort(st) ?? 0;
        const s: Server = {
          id: st.nextId++,
          name: spec.name,
          project_id: resolved[i].projectId,
          image: resolved[i].image,
          port,
          env_keys: Object.keys(spec.env ?? {}).sort(),
          desired_state: "stopped",
          state: "missing",
          container_id: "",
          started_at: null,
          exit_code: null,
          restart_required: false,
          created_at: iso(),
          updated_at: iso(),
        };
        st.servers.push(s);
        st.env.set(s.id, { ...(spec.env ?? {}) });
        return s;
      });
      return json(st, 201, { created });
    }
    if ((m = path.match(/^\/servers\/(\d+)(\/start|\/stop|\/restart|\/logs|\/stats)?$/))) {
      const s = st.servers.find((x) => x.id === Number(m?.[1]));
      if (!s) return fail(st, 404, "not_found", "server not found");
      const sub = m[2];
      if (!sub && method === "GET") return json(st, 200, s);
      if (!sub && method === "PATCH") {
        const req = body<UpdateServerRequest>();
        for (const k of req.env_unset ?? []) {
          if (req.env_set && k in req.env_set) return fail(st, 400, "validation_failed", `"${k}" is in both env_set and env_unset`, "env_unset");
        }
        const ee = envError(req.env_set, "env_set");
        if (ee) return fail(st, 400, "validation_failed", ee.message, ee.field);
        let image = s.image;
        if (req.image !== undefined) {
          const r = resolveImage(st, req.image);
          if (!r) return fail(st, 400, "validation_failed", IMAGE_MESSAGE, "image");
          if (tooManyVolumes(r)) return fail(st, 400, "validation_failed", VOLUMES_MESSAGE, "image");
          image = r;
        }
        if (req.port !== undefined) {
          const pe = portError(req.port);
          if (pe) return fail(st, 400, "validation_failed", pe, "port");
          if (st.servers.some((x) => x.id !== s.id && x.port === req.port)) return fail(st, 409, "port_in_use", "that port is already assigned to another server (or the port range is full)");
          if (req.port !== s.port) {
            if (s.state === "unknown") return fail(st, 503, "service_unavailable", "Docker is unreachable");
            if (s.state === "running" || s.state === "restarting" || s.state === "paused") return fail(st, 409, "conflict", "stop the server before changing its port");
          }
        }
        // A variable named RACE simulates a concurrent env change that keeps colliding.
        if (req.env_set && "RACE" in req.env_set) return fail(st, 409, "conflict", "the change conflicts with a concurrent change; retry");
        const env = { ...(st.env.get(s.id) ?? {}) };
        for (const k of req.env_unset ?? []) delete env[k];
        Object.assign(env, req.env_set ?? {});
        const changed = image !== s.image || (req.port !== undefined && req.port !== s.port) || Object.keys(req.env_set ?? {}).length > 0 || (req.env_unset ?? []).length > 0;
        st.env.set(s.id, env);
        s.image = image;
        if (req.port !== undefined) s.port = req.port;
        s.env_keys = Object.keys(env).sort();
        s.updated_at = iso();
        // PATCH never restarts; a running container now runs an older definition.
        if (changed && s.state === "running") s.restart_required = true;
        return json(st, 200, s);
      }
      if (!sub && method === "DELETE") {
        if (activeJobFor(st, "server", s.id)) return fail(st, 409, "job_in_progress", "another job is already running for this server");
        // A server.delete job, awaited like the daemon does; with Docker unreachable it fails and nothing is deleted.
        const lost = s.state === "unknown";
        const job = startJob(
          st,
          me,
          false,
          "server.delete",
          "server",
          s.id,
          lost ? [`deleting server "${s.name}"`, "FAIL"] : [`stopping container golem-deploy-${s.name}`, "removing container"],
          () => {},
          "Docker is unreachable",
        );
        while (isActive(job)) await sleep(200);
        if (lost) return fail(st, 503, "service_unavailable", "Docker is unreachable");
        st.servers = st.servers.filter((x) => x.id !== s.id);
        st.env.delete(s.id);
        return json(st, 204, null);
      }
      if ((sub === "/start" || sub === "/stop" || sub === "/restart") && method === "POST") {
        if (activeJobFor(st, "server", s.id)) return fail(st, 409, "job_in_progress", "another job is already running for this server");
        return json(st, 202, startServerAction(s, sub.slice(1) as "start" | "stop" | "restart", me));
      }
      if (sub === "/logs" && method === "GET") {
        let tail = 200;
        const rawTail = query.get("tail");
        if (rawTail !== null && rawTail !== "") {
          tail = Number(rawTail);
          if (!/^\d+$/.test(rawTail) || tail < 1 || tail > MAX_TAIL) return fail(st, 400, "bad_request", `tail must be an integer from 1 to ${MAX_TAIL}`);
        }
        const follow = parseFollow(query);
        if (follow === null) return fail(st, 400, "bad_request", "follow must be 1 or 0");
        if (s.state === "missing") return fail(st, 409, "conflict", "the server has no container yet; start it first");
        if (s.state === "unknown") return fail(st, 503, "service_unavailable", "Docker is unreachable");
        let release = () => {};
        if (follow) {
          const r = acquireFollow(me);
          if (r instanceof Response) return r;
          release = r;
        }
        return textStream(serverLogStream(s, tail, follow, release));
      }
      if (sub === "/stats" && method === "GET") {
        if (s.state === "missing") return fail(st, 409, "conflict", "the server has no container yet; start it first");
        if (s.state === "unknown") return fail(st, 503, "service_unavailable", "Docker is unreachable");
        if (s.state !== "running") return fail(st, 409, "conflict", "the server is not running");
        return json(st, 200, {
          cpu_percent: 5 + Math.random() * 40,
          memory_bytes: Math.round(300_000_000 + Math.random() * 50_000_000),
          memory_limit_bytes: 0,
          network_rx_bytes: Math.round((Date.now() / 10) % 1e9),
          network_tx_bytes: Math.round((Date.now() / 7) % 1e9),
          pids: 23,
          sampled_at: iso(),
        });
      }
      return null;
    }

    // Images, static files, volumes.
    if (path === "/images" && method === "GET") {
      refreshImages();
      return json(st, 200, { items: st.images });
    }
    if ((m = path.match(/^\/images\/(.+)$/)) && method === "DELETE") {
      if (!admin) return forbidden();
      refreshImages();
      const id = decodeURIComponent(m[1]);
      const img = st.images.find((i) => i.id === id);
      if (!img) return fail(st, 404, "not_found", "image not found");
      if (img.in_use) return fail(st, 409, "conflict", "the image is in use");
      st.images = st.images.filter((i) => i.id !== id);
      return json(st, 204, null);
    }
    if (path === "/static" && method === "GET") {
      return json(st, 200, staticList(st, [...st.statics.values()].map((f) => f.meta).sort((a, b) => a.name.localeCompare(b.name))));
    }
    if ((m = path.match(/^\/static\/([^/]+)$/))) {
      const name = decodeURIComponent(m[1]);
      const f = st.statics.get(name);
      if (!f) return fail(st, 404, "not_found", "file not found");
      if (method === "GET") {
        return new Response(f.blob, {
          status: 200,
          headers: headers(st, { "Content-Type": "application/octet-stream", "Content-Disposition": `attachment; filename="${name}"` }),
        });
      }
      if (method === "DELETE") {
        st.statics.delete(name);
        return json(st, 204, null);
      }
      return null;
    }
    if (path === "/volumes" && method === "GET") return json(st, 200, { items: st.volumes });
    if ((m = path.match(/^\/volumes\/([^/]+)$/)) && method === "DELETE") {
      if (!admin) return forbidden();
      return fail(st, 404, "not_found", "volume not found");
    }
    return null;
  }

  function startServerAction(s: Server, action: "start" | "stop" | "restart", me: Me): Job {
    s.desired_state = action === "stop" ? "stopped" : "running";
    s.updated_at = iso();
    const steps = action === "stop" ? [`stopping container golem-deploy-${s.name}`] : ["ensuring network golem-deploy", `starting container golem-deploy-${s.name}`, "container is running"];
    if (s.name === "crashy" && action !== "stop") {
      steps.push("container | panic: listen udp :7777: \\x1b[31mbind: address already in use\\x1b[0m", "container | exit status 1", "FAIL");
    }
    if (s.state === "unknown") steps.splice(0, steps.length, `${action} server "${s.name}"`, "FAIL");
    const failDetail = s.state === "unknown" ? "Docker is unreachable" : "the container exited right after starting (exit code 1); its last output is in the job log";
    return startJob(st, me, false, `server.${action}`, "server", s.id, steps, (ok) => {
      if (action === "stop") {
        if (ok && s.state !== "unknown") Object.assign(s, { state: "exited", exit_code: 0, started_at: null, restart_required: false });
        return;
      }
      if (ok) Object.assign(s, { state: "running", exit_code: null, started_at: iso(), restart_required: false, container_id: s.container_id || `c0ffee${s.id}0000` });
      else if (s.state !== "unknown") Object.assign(s, { state: "exited", exit_code: 1, started_at: null, container_id: s.container_id || `c0ffee${s.id}0000` });
    }, failDetail);
  }

  return {
    async fetch(url, init) {
      const u = new URL(url, window.location.origin);
      if (u.pathname.startsWith(BASE)) {
        const res = await handle((init.method ?? "GET").toUpperCase(), u.pathname.slice(BASE.length), u.searchParams, init);
        if (res) {
          await sleep(120);
          if (init.signal?.aborted) throw new DOMException("aborted", "AbortError");
          return res;
        }
      }
      return real.fetch(url, init);
    },

    async upload(req: UploadRequest): Promise<RawResponse> {
      const u = new URL(req.url, window.location.origin);
      if (u.pathname !== `${BASE}/static`) return real.upload(req);
      const me = await identify(req);
      if (!me) return real.upload(req);
      const inflight = st.uploads.get(me.id) ?? 0;
      if (inflight >= MAX_UPLOADS) {
        const { h, text } = errorBody(st, "rate_limited", "too many uploads in progress for this account; wait for one to finish", undefined, { "Retry-After": "1" });
        return { status: 429, headers: h, bodyText: text };
      }
      st.uploads.set(me.id, inflight + 1);
      try {
        return await storeUpload(req, u);
      } finally {
        st.uploads.set(me.id, Math.max(0, (st.uploads.get(me.id) ?? 1) - 1));
      }
    },
  };

  /** POST /static after the concurrency check: every check passes for all files, or nothing is stored (Atomicity). */
  async function storeUpload(req: UploadRequest, u: URL): Promise<RawResponse> {
    const files = req.body.getAll("files").filter((f): f is File => f instanceof File);
    const total = files.reduce((n, f) => n + f.size, 0) || 1;
    for (let i = 1; i <= 20; i++) {
      if (req.signal?.aborted) throw new DOMException("aborted", "AbortError");
      await sleep(Math.min(150, 20 + total / 2_000_000));
      req.onProgress?.(Math.round((total * i) / 20), total);
    }
    const raw = (status: number, bodyText: string, h: Headers): RawResponse => ({ status, headers: h, bodyText });
    const failRaw = (status: number, code: ErrorCode, message: string, field?: string) => {
      const { h, text } = errorBody(st, code, message, field);
      return raw(status, text, h);
    };
    const replace = u.searchParams.get("replace");
    if (replace !== null && !["", "0", "1", "true", "false"].includes(replace)) return failRaw(400, "bad_request", "replace must be 1 or 0");
    if (files.length === 0) return failRaw(400, "validation_failed", "no files were uploaded", "files");
    for (const f of files) {
      const e = validateStaticName(f.name);
      if (e) return failRaw(400, "validation_failed", e, "files");
    }
    if (!(replace === "1" || replace === "true") && files.some((f) => st.statics.has(f.name))) {
      return failRaw(409, "conflict", "a file with that name already exists; upload with replace=1 to overwrite it");
    }
    const replaced = files.reduce((n, f) => n + (st.statics.get(f.name)?.meta.size_bytes ?? 0), 0);
    const used = [...st.statics.values()].reduce((n, f) => n + f.meta.size_bytes, 0);
    if (used - replaced + files.reduce((n, f) => n + f.size, 0) > STATIC_QUOTA) {
      return failRaw(507, "insufficient_storage", `the upload would exceed the static store quota of ${STATIC_QUOTA / 1024 / 1024} MiB`);
    }
    if (st.statics.size + files.filter((f) => !st.statics.has(f.name)).length > STATIC_MAX_FILES) {
      return failRaw(507, "insufficient_storage", `the static store holds at most ${STATIC_MAX_FILES} files`);
    }
    const stored = await Promise.all(
      files.map(async (f) => {
        const head = new Uint8Array(await f.slice(0, 4).arrayBuffer());
        const zip = f.name.toLowerCase().endsWith(".zip") && head[0] === 0x50 && head[1] === 0x4b;
        const meta: StaticFile = { name: f.name, size_bytes: f.size, modified_at: iso(), is_zip: zip };
        st.statics.set(f.name, { meta, blob: f });
        return meta;
      }),
    );
    const h = headers(st, { "Content-Type": "application/json" });
    return raw(201, JSON.stringify(staticList(st, stored)), h);
  }
}
