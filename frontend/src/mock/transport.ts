/**
 * Dev-only mock of the routes the daemon does not serve yet (projects,
 * servers, images, static files, volumes, and the jobs they start), shaped
 * by the generated wire types. Everything else (auth, account, admin,
 * status, real jobs) passes through to the daemon.
 *
 * Enabled with VITE_API_MOCK=1 under `vite` (dev). It is imported only behind
 * `import.meta.env.DEV`, so production builds drop it; vite.config.ts fails
 * a production build that has the flag set or that contains MOCK_MARKER.
 */
import type { RawResponse, Transport, UploadRequest } from "@/api/client";
import type {
  CreateProjectRequest,
  CreateServerRequest,
  ErrorCode,
  Image,
  Job,
  JobKind,
  Project,
  Server,
  ServerExport,
  StaticFile,
  SubjectType,
  SwitchBranchRequest,
  UpdateProjectRequest,
  UpdateServerRequest,
  Volume,
} from "@/api/types.gen";

export const MOCK_MARKER = "golem-deploy-mock-layer";

const BASE = "/api/v1";
const NAME_RE = /^[a-z0-9][a-z0-9-]{0,62}$/;
const ENV_KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;
const STATIC_RE = /^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$/;
const FLOOD_LINES = 50_000;

interface MockState {
  projects: Project[];
  servers: Server[];
  env: Map<number, Record<string, string>>;
  images: Image[];
  statics: Map<string, { meta: StaticFile; blob: Blob }>;
  volumes: Volume[];
  jobs: Job[];
  jobLogs: Map<number, string[]>;
  nextId: number;
  nextJobId: number;
  requestSeq: number;
}

const iso = (msAgo = 0) => new Date(Date.now() - msAgo).toISOString();
const HOUR = 3_600_000;
const DAY = 24 * HOUR;

function seed(): MockState {
  const projects: Project[] = [
    {
      id: 1,
      name: "arena-shooter",
      repo_url: "https://github.com/example/arena-shooter.git",
      branch: "main",
      auth_type: "none",
      auth_username: "",
      has_secret: false,
      last_commit: "4f1c2a9e0b7d3c5a8e6f1b2c3d4e5f6a7b8c9d0e",
      last_pulled_at: iso(2 * HOUR),
      status: "idle",
      created_at: iso(20 * DAY),
      updated_at: iso(2 * HOUR),
    },
    {
      id: 2,
      name: "kart-racer",
      repo_url: "git@gitlab.example.com:studio/kart-racer.git",
      branch: "develop",
      auth_type: "ssh",
      auth_username: "",
      has_secret: true,
      last_commit: "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b",
      last_pulled_at: iso(3 * DAY),
      status: "idle",
      created_at: iso(40 * DAY),
      updated_at: iso(3 * DAY),
    },
    {
      id: 3,
      name: "private-sandbox",
      repo_url: "https://git.example.com/team/sandbox.git",
      branch: "main",
      auth_type: "https",
      auth_username: "deploy-bot",
      has_secret: true,
      last_commit: "",
      last_pulled_at: null,
      status: "error",
      created_at: iso(DAY),
      updated_at: iso(DAY),
    },
  ];
  const server = (
    id: number,
    name: string,
    port: number,
    desired: Server["desired_state"],
    state: Server["state"],
    extra: Partial<Server> = {},
  ): Server => ({
    id,
    name,
    project_id: 1,
    image: "golem/arena-shooter:4f1c2a9",
    port,
    env_keys: ["MAX_PLAYERS", "ADMIN_TOKEN"],
    desired_state: desired,
    state,
    container_id: state === "missing" ? "" : `c0ffee${id}a1b2c3d4e5f60718293a4b5c6d7e8f90`,
    started_at: state === "running" ? iso(5 * HOUR) : null,
    exit_code: state === "exited" ? 0 : null,
    created_at: iso(10 * DAY),
    updated_at: iso(6 * HOUR),
    ...extra,
  });
  const servers: Server[] = [
    server(11, "eu-arena", 7777, "running", "running"),
    server(12, "staging", 7778, "stopped", "exited"),
    server(13, "crashy", 7779, "running", "exited", { exit_code: 137, updated_at: iso(HOUR) }),
    server(14, "flood-test", 7780, "running", "running", { env_keys: [] }),
    server(15, "kart-eu", 7781, "running", "running", {
      project_id: 2,
      image: "golem/kart-racer:9a8b7c6",
      started_at: iso(3 * HOUR),
      updated_at: iso(HOUR),
    }),
  ];
  const env = new Map<number, Record<string, string>>();
  for (const s of servers) env.set(s.id, Object.fromEntries(s.env_keys.map((k) => [k, "secret"])));
  const images: Image[] = [
    { id: "sha256:4f1c2a9e0b7d3c5a8e6f1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e", tags: ["golem/arena-shooter:4f1c2a9", "golem/arena-shooter:latest"], project_id: 1, size_bytes: 187_400_000, created_at: iso(2 * HOUR), in_use: true },
    { id: "sha256:1111111111111111111111111111111111111111111111111111111111111111", tags: ["golem/arena-shooter:0d3e2f1"], project_id: 1, size_bytes: 185_100_000, created_at: iso(9 * DAY), in_use: false },
    { id: "sha256:9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9a8b7c6d5e4f3a2b1c0d9e8f", tags: ["golem/kart-racer:9a8b7c6"], project_id: 2, size_bytes: 243_900_000, created_at: iso(3 * DAY), in_use: true },
  ];
  const statics = new Map<string, { meta: StaticFile; blob: Blob }>();
  for (const [name, text] of [
    ["maps-pack-v3.zip", "pretend zip"],
    ["sound_bank.bin", "pretend audio"],
  ] as const) {
    const blob = new Blob([text]);
    statics.set(name, { meta: { name, size_bytes: name.endsWith(".zip") ? 48_300_000 : 9_100_000, sha256: "3b5d5c3712955042212316173ccf37be8000000000000000000000000000000".slice(0, 64), modified_at: iso(5 * DAY) }, blob });
  }
  const volumes: Volume[] = [
    { name: "golem-server-11-data", server_id: 11, server_name: "eu-arena", created_at: iso(10 * DAY), in_use: true },
    { name: "golem-server-12-data", server_id: 12, server_name: "staging", created_at: iso(10 * DAY), in_use: false },
    { name: "golem-server-7-data", server_id: null, server_name: "old-test", created_at: iso(30 * DAY), in_use: false },
  ];
  return { projects, servers, env, images, statics, volumes, jobs: [], jobLogs: new Map(), nextId: 100, nextJobId: 900_001, requestSeq: 0 };
}

function headers(st: MockState, extra: Record<string, string> = {}): Headers {
  st.requestSeq++;
  return new Headers({ "X-Request-Id": `mock-${st.requestSeq.toString(16).padStart(6, "0")}`, ...extra });
}

function json(st: MockState, status: number, body: unknown): Response {
  if (status === 204) return new Response(null, { status, headers: headers(st) });
  return new Response(JSON.stringify(body), { status, headers: headers(st, { "Content-Type": "application/json" }) });
}

function fail(st: MockState, status: number, code: ErrorCode, message: string, field?: string): Response {
  const h = headers(st, { "Content-Type": "application/json" });
  return new Response(
    JSON.stringify({ error: { code, message, request_id: h.get("X-Request-Id"), ...(field ? { field } : {}) } }),
    { status, headers: h },
  );
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function activeJobFor(st: MockState, type: SubjectType, id: number): Job | undefined {
  return st.jobs.find((j) => j.subject_type === type && j.subject_id === String(id) && (j.status === "queued" || j.status === "running"));
}

/** Starts a fake job that logs a few lines per step and then runs `finish`. */
function startJob(st: MockState, kind: JobKind, type: SubjectType, id: number, steps: string[], finish: (ok: boolean) => void): Job {
  const job: Job = {
    id: st.nextJobId++,
    kind,
    subject_type: type,
    subject_id: String(id),
    status: "queued",
    detail: "",
    created_by: null,
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
        if (step !== "FAIL") {
          for (let n = 0; n < 4; n++) log.push(`[${new Date().toISOString()}] ${step}${n ? ` (${n}/3)` : ""}`);
        }
        return;
      }
      clearInterval(tick);
      job.status = failing ? "error" : "success";
      job.detail = failing ? "authentication failed for the remote repository" : "";
      job.finished_at = iso();
      log.push(failing ? "fatal: Authentication failed" : "done");
      finish(!failing);
    }, 500);
  }, 400);
  return job;
}

function serverSteps(action: string, name: string): string[] {
  return [`${action} container golem-${name}`, "waiting for container", `container ${action === "stop" ? "stopped" : "running"}`];
}

function cancelJob(job: Job) {
  job.status = "cancelled";
  job.detail = "cancelled by user";
  job.finished_at = iso();
}

function streamLines(produce: (push: (lines: string[]) => void, close: () => void) => () => void): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  let stop: () => void = () => {};
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
          }
        },
      );
    },
    cancel() {
      stop();
    },
  });
}

function jobLogStream(st: MockState, job: Job, follow: boolean): ReadableStream<Uint8Array> {
  return streamLines((push, close) => {
    const log = st.jobLogs.get(job.id) ?? [];
    let sent = 0;
    const drain = () => {
      push(log.slice(sent));
      sent = log.length;
    };
    drain();
    if (!follow) {
      close();
      return () => {};
    }
    const t = setInterval(() => {
      drain();
      if (job.status !== "queued" && job.status !== "running") {
        clearInterval(t);
        close();
      }
    }, 250);
    return () => clearInterval(t);
  });
}

function serverLogStream(server: Server, tail: number, follow: boolean): ReadableStream<Uint8Array> {
  const line = (n: number) =>
    `${new Date().toISOString()} [${n % 17 === 0 ? "WARN" : "INFO"}] ${server.name}: tick ${n} players=${(n * 7) % 33} rtt=${(n * 13) % 90}ms` +
    (n % 101 === 0 ? " \u001b[31mcoloured output stripped in the viewer\u001b[0m snowman ☃ and emoji-free unicode éè" : "");
  return streamLines((push, close) => {
    let n = 0;
    const timers: ReturnType<typeof setInterval>[] = [];
    if (server.name === "flood-test") {
      // 50k lines as fast as possible, to exercise windowing and the buffer cap.
      const t = setInterval(() => {
        const batch: string[] = [];
        for (let i = 0; i < 2000 && n < FLOOD_LINES; i++) batch.push(line(n++));
        push(batch);
        if (n >= FLOOD_LINES) {
          clearInterval(t);
          if (!follow) close();
        }
      }, 15);
      timers.push(t);
    } else {
      const history: string[] = [];
      for (let i = 0; i < Math.min(tail, 400); i++) history.push(line(n++));
      push(history);
      if (!follow || server.state !== "running") {
        close();
        return () => {};
      }
    }
    if (follow) {
      timers.push(setInterval(() => push([line(n++)]), 700));
    }
    return () => timers.forEach(clearInterval);
  });
}

function validEnv(env: Record<string, string> | undefined): string | null {
  for (const k of Object.keys(env ?? {})) if (!ENV_KEY_RE.test(k)) return k;
  return null;
}

function nextPort(st: MockState): number {
  let p = 7777;
  while (st.servers.some((s) => s.port === p)) p++;
  return p;
}

export function createMockTransport(real: Transport): Transport {
  const st = seed();
  console.info(`${MOCK_MARKER}: VITE_API_MOCK=1, projects/servers/images/static/volumes are simulated in the browser.`);

  async function handle(method: string, path: string, query: URLSearchParams, init: RequestInit): Promise<Response | null> {
    const body = <T>(): T => JSON.parse(typeof init.body === "string" ? init.body : "{}") as T;
    let m: RegExpMatchArray | null;

    // Jobs that belong to mocked subjects.
    if (method === "GET" && path === "/jobs") {
      const type = query.get("subject_type");
      if (type !== "project" && type !== "server") return null;
      const sid = query.get("subject_id");
      const limit = Number(query.get("limit") ?? 50);
      return json(st, 200, { items: st.jobs.filter((j) => j.subject_type === type && (!sid || j.subject_id === sid)).slice(0, limit) });
    }
    if ((m = path.match(/^\/jobs\/(\d+)(\/log|\/cancel)?$/))) {
      const job = st.jobs.find((j) => j.id === Number(m?.[1]));
      if (!job) return null;
      if (method === "GET" && !m[2]) return json(st, 200, job);
      if (method === "POST" && m[2] === "/cancel") {
        if (job.status !== "queued" && job.status !== "running") return fail(st, 409, "job_not_active", "job has already finished");
        cancelJob(job);
        return json(st, 202, job);
      }
      if (method === "GET" && m[2] === "/log") {
        return new Response(jobLogStream(st, job, query.get("follow") === "1"), { status: 200, headers: headers(st, { "Content-Type": "text/plain; charset=utf-8" }) });
      }
    }

    // Projects.
    if (path === "/projects" && method === "GET") return json(st, 200, { items: st.projects });
    if (path === "/projects" && method === "POST") {
      const req = body<CreateProjectRequest>();
      if (!NAME_RE.test(req.name)) return fail(st, 400, "validation_failed", "name must match ^[a-z0-9][a-z0-9-]{0,62}$", "name");
      if (st.projects.some((p) => p.name === req.name)) return fail(st, 409, "name_taken", "a project with that name exists");
      const p: Project = {
        id: st.nextId++,
        name: req.name,
        repo_url: req.repo_url,
        branch: req.branch || "main",
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
      if (req.repo_url.includes("fail")) steps.push("FAIL");
      const job = startJob(st, "project.clone", "project", p.id, steps, (ok) => {
        p.status = ok ? "idle" : "error";
        if (ok) {
          p.last_commit = "0123456789abcdef0123456789abcdef01234567";
          p.last_pulled_at = iso();
        }
      });
      return json(st, 201, { project: p, job });
    }
    if ((m = path.match(/^\/projects\/(\d+)(\/pull|\/branch|\/branches)?$/))) {
      const p = st.projects.find((x) => x.id === Number(m?.[1]));
      if (!p) return fail(st, 404, "not_found", "project not found");
      const sub = m[2];
      if (!sub && method === "GET") return json(st, 200, p);
      if (!sub && method === "PATCH") {
        const req = body<UpdateProjectRequest>();
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
        st.projects = st.projects.filter((x) => x.id !== p.id);
        for (const s of st.servers) if (s.project_id === p.id) s.project_id = null;
        return json(st, 204, null);
      }
      if (sub === "/branches" && method === "GET") return json(st, 200, { current: p.branch, items: ["develop", "feature/netcode", "main"] });
      if ((sub === "/pull" || sub === "/branch") && method === "POST") {
        if (activeJobFor(st, "project", p.id)) return fail(st, 409, "job_in_progress", "another job is already running for this project");
        if (sub === "/pull") {
          p.status = "pulling";
          return json(
            st,
            202,
            startJob(st, "project.pull", "project", p.id, ["Fetching origin", "Fast-forwarding", "Updating submodules"], (ok) => {
              p.status = ok ? "idle" : "error";
              p.last_pulled_at = iso();
              p.last_commit = Math.random().toString(16).slice(2).padEnd(40, "0").slice(0, 40);
            }),
          );
        }
        const { branch } = body<SwitchBranchRequest>();
        p.status = "switching_branch";
        return json(
          st,
          202,
          startJob(st, "project.branch", "project", p.id, [`Fetching ${branch}`, `Switched to branch '${branch}'`], (ok) => {
            p.status = ok ? "idle" : "error";
            if (ok) p.branch = branch;
          }),
        );
      }
    }

    // Servers.
    if (path === "/servers" && method === "GET") return json(st, 200, { items: st.servers });
    if (path === "/servers" && method === "POST") {
      const req = body<CreateServerRequest>();
      if (!NAME_RE.test(req.name)) return fail(st, 400, "validation_failed", "name must match ^[a-z0-9][a-z0-9-]{0,62}$", "name");
      if (st.servers.some((s) => s.name === req.name)) return fail(st, 409, "name_taken", "a server with that name exists");
      if (req.port !== undefined && st.servers.some((s) => s.port === req.port)) return fail(st, 409, "port_in_use", "port is already assigned");
      const bad = validEnv(req.env);
      if (bad !== null) return fail(st, 400, "validation_failed", `invalid environment variable name ${bad}`, "env");
      const s: Server = {
        id: st.nextId++,
        name: req.name,
        project_id: req.project_id ?? null,
        image: req.image,
        port: req.port ?? nextPort(st),
        env_keys: Object.keys(req.env ?? {}).sort(),
        desired_state: req.desired_state ?? "stopped",
        state: "missing",
        container_id: "",
        started_at: null,
        exit_code: null,
        created_at: iso(),
        updated_at: iso(),
      };
      st.servers.push(s);
      st.env.set(s.id, { ...(req.env ?? {}) });
      if (s.desired_state === "running") {
        startJob(st, "server.start", "server", s.id, serverSteps("start", s.name), (ok) => {
          if (ok) Object.assign(s, { state: "running", started_at: iso(), container_id: `c0ffee${s.id}0000` });
        });
      }
      return json(st, 201, s);
    }
    if (path === "/servers/export" && method === "GET") {
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
      const req = body<ServerExport>();
      for (const spec of req.servers) {
        if (st.servers.some((s) => s.name === spec.name)) return fail(st, 409, "name_taken", `server ${spec.name} already exists`);
        if (spec.port && st.servers.some((s) => s.port === spec.port)) return fail(st, 409, "port_in_use", `port ${spec.port} is already assigned`);
      }
      const created = req.servers.map((spec) => {
        const s: Server = {
          id: st.nextId++,
          name: spec.name,
          project_id: st.projects.find((p) => p.name === spec.project)?.id ?? null,
          image: spec.image,
          port: spec.port ?? nextPort(st),
          env_keys: Object.keys(spec.env ?? {}).sort(),
          desired_state: spec.desired_state,
          state: "missing",
          container_id: "",
          started_at: null,
          exit_code: null,
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
        const env = st.env.get(s.id) ?? {};
        for (const k of req.env_unset ?? []) {
          if (req.env_set && k in req.env_set) return fail(st, 400, "validation_failed", `${k} is in both env_set and env_unset`, "env_unset");
          delete env[k];
        }
        const bad = validEnv(req.env_set);
        if (bad !== null) return fail(st, 400, "validation_failed", `invalid environment variable name ${bad}`, "env_set");
        Object.assign(env, req.env_set ?? {});
        st.env.set(s.id, env);
        if (req.port !== undefined) {
          if (st.servers.some((x) => x.id !== s.id && x.port === req.port)) return fail(st, 409, "port_in_use", "port is already assigned");
          s.port = req.port;
        }
        if (req.image !== undefined) s.image = req.image;
        s.env_keys = Object.keys(env).sort();
        s.updated_at = iso();
        return json(st, 200, s);
      }
      if (!sub && method === "DELETE") {
        st.servers = st.servers.filter((x) => x.id !== s.id);
        for (const v of st.volumes) if (v.server_id === s.id) Object.assign(v, { server_id: null, in_use: false });
        return json(st, 204, null);
      }
      if ((sub === "/start" || sub === "/stop" || sub === "/restart") && method === "POST") {
        if (activeJobFor(st, "server", s.id)) return fail(st, 409, "job_in_progress", "another job is already running for this server");
        const action = sub.slice(1) as "start" | "stop" | "restart";
        s.desired_state = action === "stop" ? "stopped" : "running";
        s.updated_at = iso();
        return json(
          st,
          202,
          startJob(st, `server.${action}`, "server", s.id, serverSteps(action, s.name), (ok) => {
            if (!ok) return;
            if (action === "stop") Object.assign(s, { state: "exited", exit_code: 0, started_at: null });
            else Object.assign(s, { state: "running", exit_code: null, started_at: iso(), container_id: s.container_id || `c0ffee${s.id}0000` });
          }),
        );
      }
      if (sub === "/logs" && method === "GET") {
        if (s.state === "missing") return fail(st, 409, "conflict", "the server has no container");
        const tail = Number(query.get("tail") ?? 100);
        return new Response(serverLogStream(s, tail, query.get("follow") === "1"), {
          status: 200,
          headers: headers(st, { "Content-Type": "text/plain; charset=utf-8" }),
        });
      }
      if (sub === "/stats" && method === "GET") {
        if (s.state !== "running") return fail(st, 409, "conflict", "the server is not running");
        return json(st, 200, {
          cpu_percent: 5 + Math.random() * 40,
          memory_bytes: Math.round(300_000_000 + Math.random() * 50_000_000),
          memory_limit_bytes: 2_147_483_648,
          network_rx_bytes: Math.round((Date.now() / 10) % 1e9),
          network_tx_bytes: Math.round((Date.now() / 7) % 1e9),
          pids: 23,
          sampled_at: iso(),
        });
      }
    }

    // Images, static files, volumes.
    if (path === "/images" && method === "GET") {
      for (const img of st.images) img.in_use = st.servers.some((s) => img.tags.includes(s.image) && s.state !== "missing");
      return json(st, 200, { items: st.images });
    }
    if ((m = path.match(/^\/images\/(.+)$/)) && method === "DELETE") {
      const id = decodeURIComponent(m[1]);
      const img = st.images.find((i) => i.id === id);
      if (!img) return fail(st, 404, "not_found", "image not found");
      if (st.servers.some((s) => img.tags.includes(s.image))) return fail(st, 409, "conflict", "image is in use by a server");
      st.images = st.images.filter((i) => i.id !== id);
      return json(st, 204, null);
    }
    if (path === "/static" && method === "GET") return json(st, 200, { items: [...st.statics.values()].map((f) => f.meta) });
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
    }
    if (path === "/volumes" && method === "GET") return json(st, 200, { items: st.volumes });
    if ((m = path.match(/^\/volumes\/([^/]+)$/)) && method === "DELETE") {
      const name = decodeURIComponent(m[1]);
      const v = st.volumes.find((x) => x.name === name);
      if (!v) return fail(st, 404, "not_found", "volume not found");
      if (v.in_use) return fail(st, 409, "conflict", "volume is in use");
      st.volumes = st.volumes.filter((x) => x.name !== name);
      return json(st, 204, null);
    }
    return null;
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
      const files = req.body.getAll("files").filter((f): f is File => f instanceof File);
      const total = files.reduce((n, f) => n + f.size, 0) || 1;
      const steps = 20;
      for (let i = 1; i <= steps; i++) {
        if (req.signal?.aborted) throw new DOMException("aborted", "AbortError");
        await sleep(Math.min(150, 20 + total / 2_000_000));
        req.onProgress?.(Math.round((total * i) / steps), total);
      }
      const h = headers(st, { "Content-Type": "application/json" });
      const raw = (status: number, b: unknown): RawResponse => ({ status, headers: h, bodyText: JSON.stringify(b) });
      for (const f of files) {
        if (!STATIC_RE.test(f.name)) {
          return raw(400, { error: { code: "validation_failed", message: `invalid file name ${f.name}`, request_id: h.get("X-Request-Id"), field: "files" } });
        }
      }
      const stored = files.map((f) => {
        const meta: StaticFile = { name: f.name, size_bytes: f.size, sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", modified_at: iso() };
        st.statics.set(f.name, { meta, blob: f });
        return meta;
      });
      return raw(201, { items: stored });
    },
  };
}
