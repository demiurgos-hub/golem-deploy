/**
 * Dev-only stand-in for the Docker-, git- and file-backed routes (projects,
 * builds, servers and their runtime view and saved data, backups, images,
 * static files, volumes, the certificate settings, the Inspector and their
 * jobs), so the
 * web client can be developed
 * without Docker, git or network access. It follows the API v1 contract
 * (docs/API.md) and the generated wire types: access levels, restricted
 * jobs, error codes and messages, the follow stream cap, tail bounds, static
 * replace semantics, build and deploy rules and import validation.
 * Everything else (auth, account, admin, settings, status, other jobs) and
 * the caller's identity come from the real daemon; the game host is the
 * page's host name.
 *
 * Enabled with VITE_API_MOCK=1 under `vite` (dev). It is imported only behind
 * `import.meta.env.DEV`, so production builds drop it; vite.config.ts fails
 * a production build that has the flag set or that contains MOCK_MARKER.
 */
import type { RawResponse, Transport, UploadRequest } from "@/api/client";
import type {
  ACME,
  ACMECertificateUse,
  Backup,
  BackupKind,
  BackupSchedule,
  Build,
  BuildStatus,
  CertificateInfo,
  CertificateMode,
  CreateBackupRequest,
  CreateBuildRequest,
  CreateInspectorSessionRequest,
  CreateProjectRequest,
  CreateServerRequest,
  CurrentAccount,
  ErrorCode,
  GameCertificate,
  Image,
  Inspector,
  InspectorSession,
  InspectorTarget,
  Job,
  JobKind,
  PanelCertificate,
  Project,
  ProjectAuth,
  ResetServerDataRequest,
  RestoreServerRequest,
  Server,
  ServerConnectivity,
  ServerExport,
  ServerRuntime,
  StaticAddon,
  StaticFile,
  SubjectType,
  SwitchBranchRequest,
  UpdateACMERequest,
  UpdateGameCertificateRequest,
  UpdateInspectorRequest,
  UpdatePanelCertificateRequest,
  UpdateProjectRequest,
  UpdateServerRequest,
  Volume,
} from "@/api/types.gen";
import {
  BACKUP_NOTE_FORBIDDEN,
  BACKUP_NOTE_MAX,
  checkRepoUrl,
  sameRepoOrigin,
  trimBackupNote,
  validateEnvKey,
  validateEnvTotals,
  validateEnvValue,
  validateStaticName,
} from "@/lib/validators";

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
const MAX_ACTIVE_BUILDS = 4;
const MAX_ADDONS = 16;
const MAX_ORIGINS = 16;
const BACKUP_MAX_CONTENT = 2048 * 1024 * 1024;

interface MockState {
  projects: Project[];
  builds: Build[];
  servers: Server[];
  /** Per-server self-signed certificates (created by the first start). */
  serverCerts: Map<number, CertificateInfo>;
  certMode: CertificateMode;
  customCert: CertificateInfo | null;
  panelMode: PanelCertificate["mode"];
  panelSelfSigned: CertificateInfo;
  acme: ACME;
  env: Map<number, Record<string, string>>;
  images: Image[];
  statics: Map<string, { meta: StaticFile; blob: Blob }>;
  volumes: Volume[];
  backups: Backup[];
  /** The backup each restore job reads (it cannot be deleted while the job runs). */
  restoreReads: Map<number, number>;
  /** Entries of a server's data directory that a backup leaves out (links, pipes, devices, names not allowed), by server id. */
  omittedEntries: Map<number, number>;
  /** The installation's Inspector. */
  inspector: MockInspector;
  /** The Inspector secrets each server's current container was started with (servers started with the Inspector). */
  inspectorTokens: Map<number, number>;
  /** Running servers whose definition changed apart from the Inspector (env, build, certificate mode, ...): restart_required until their next start. */
  restartPending: Set<number>;
  /** Expiry times (ms) of the entry links each account holds, by account id (a link is never used in the mock). */
  inspectorLinks: Map<number, number[]>;
  /** Called after every job's own finish step. */
  onJobEnd?: () => void;
  jobs: Job[];
  jobLogs: Map<number, string[]>;
  follows: Map<number, number>;
  uploads: Map<number, number>;
  nextId: number;
  nextBuildId: number;
  nextJobId: number;
  nextBackupId: number;
  volumeSeq: number;
  requestSeq: number;
}

interface MockInspector {
  enabled: boolean;
  state: Inspector["state"];
  detail: string;
  image: string;
  imageId: string;
  startedAt: string | null;
  /** Bumped by rotate_secrets: containers started with older secrets cannot connect. */
  secrets: number;
  /** Whether the host has the Inspector's image (turning the Inspector off keeps it). */
  imagePresent: boolean;
  /** Target names the hub saw connected at the last look, and when a target left (ms): the hub lists it a while longer. */
  connected: Set<string>;
  left: Map<string, number>;
}

type Me = Pick<CurrentAccount, "id" | "username" | "role" | "auth">;

const iso = (msAgo = 0) => new Date(Date.now() - msAgo).toISOString();
const HOUR = 3_600_000;
const DAY = 24 * HOUR;
const ACME_PRODUCTION = "https://acme-v02.api.letsencrypt.org/directory";
const ACME_STAGING = "https://acme-staging-v02.api.letsencrypt.org/directory";
const IMG_ARENA = "sha256:4f1c2a9e0b7d3c5a8e6f1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e";
const IMG_ARENA_OLD = "sha256:1111111111111111111111111111111111111111111111111111111111111111";
const IMG_KART = "sha256:9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9a8b7c6d5e4f3a2b1c0d9e8f";
// Left behind by a build whose image could not be removed: no build records it.
const IMG_ORPHAN = "sha256:5555555555555555555555555555555555555555555555555555555555555555";
const COMMIT_ARENA = "4f1c2a9e0b7d3c5a8e6f1b2c3d4e5f6a7b8c9d0e";
const COMMIT_ARENA_OLD = "0d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e";
const COMMIT_KART = "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b";
const ENGINE = "v0.4.0-rc.1";
const FINGERPRINT = "3b6f0c2a91d4e8f7";
const FINGERPRINT_OLD = "a0c3d9e1f2b47c55";
// The daemon's defaults (--inspector-image, --inspector-listen).
const INSPECTOR_IMAGE = "ghcr.io/demiurgos-hub/golem-inspector:0.4.0-rc.1";
const INSPECTOR_IMAGE_ID = "sha256:7e1d0c9b8a7f6e5d4c3b2a1908f7e6d5c4b3a29180f7e6d5c4b3a2918f7e6d5c";
const INSPECTOR_PORT = 8444;
/** How long the hub keeps listing a target after its game left. */
const INSPECTOR_TARGET_LINGER_MS = 30_000;
/** Unused entry links one account may hold (the daemon's maxTicketsPerAccount); each works for 60 seconds. */
const INSPECTOR_LINKS_PER_ACCOUNT = 8;
const INSPECTOR_LINK_MS = 60_000;
/** The Docker network gateway the games' Inspector agents connect to. */
const INSPECTOR_AGENT_ADDRESS = `172.18.0.1:${INSPECTOR_PORT}`;
/** Games whose engine has no Inspector support: started with it, they never connect. */
const NO_INSPECTOR_SUPPORT = new Set(["flood-test"]);

/** The game host servers use: the page's host name (the real one comes from the daemon's settings). */
function gameHost(): string {
  return typeof window !== "undefined" && window.location.hostname ? window.location.hostname : "localhost";
}

function fakeFingerprint(seed: string): string {
  let h = 2166136261;
  let out = "";
  for (let i = 0; out.length < 64; i++) {
    h = Math.imul(h ^ seed.charCodeAt(i % Math.max(1, seed.length)) ^ i, 16777619) >>> 0;
    out += h.toString(16).padStart(8, "0");
  }
  return out.slice(0, 64);
}

/** A self-signed certificate like the daemon generates per server: 13 days, game host, localhost and loopback. */
function selfSignedCert(seed: string, ageMs = 0): CertificateInfo {
  const host = gameHost();
  const from = Date.now() - ageMs - HOUR;
  return {
    subject: `CN=${host}`,
    issuer: `CN=${host}`,
    dns_names: [...new Set([host, "localhost"])].filter((h) => !/^[\d.:]+$/.test(h)),
    ip_addresses: ["127.0.0.1", "::1"],
    not_before: new Date(from).toISOString(),
    not_after: new Date(from + 13 * DAY).toISOString(),
    fingerprint_sha256: fakeFingerprint(seed),
    self_signed: true,
    chain_length: 1,
    covers_game_host: true,
  };
}

function seed(): MockState {
  const project = (id: number, name: string, repo: string, extra: Partial<Project>): Project => ({
    id,
    name,
    repo_url: repo,
    branch: "main",
    auth_type: "none",
    auth_username: "",
    has_secret: false,
    last_commit: COMMIT_ARENA,
    last_pulled_at: iso(2 * HOUR),
    status: "idle",
    static_addons: [],
    allow_custom_dockerfile: false,
    created_at: iso(20 * DAY),
    updated_at: iso(2 * HOUR),
    ...extra,
  });
  const projects: Project[] = [
    project(1, "arena-shooter", "https://github.com/example/arena-shooter.git", {
      static_addons: [{ name: "maps-pack-v3.zip", target: "assets/maps" }],
    }),
    // Its newest build failed the engine version check.
    project(2, "kart-racer", "git@gitlab.example.com:studio/kart-racer.git", {
      branch: "develop",
      auth_type: "ssh",
      has_secret: true,
      last_commit: COMMIT_KART,
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

  const jobs: Job[] = [];
  const jobLogs = new Map<number, string[]>();
  let nextJobId = 900_001;
  const build = (id: number, projectId: number, status: BuildStatus, ageMs: number, extra: Partial<Build>, log: string[]): Build => {
    const p = projects.find((x) => x.id === projectId);
    const job: Job = {
      id: nextJobId++,
      kind: "project.build",
      subject_type: "project",
      subject_id: String(projectId),
      status,
      detail: extra.detail ?? "",
      restricted: false,
      created_by: null,
      created_by_username: null,
      created_at: iso(ageMs),
      started_at: iso(ageMs - 1000),
      finished_at: iso(ageMs - 4 * 60_000),
    };
    jobs.push(job);
    jobLogs.set(job.id, log);
    const ok = status === "success";
    const commit = extra.commit ?? p?.last_commit ?? "";
    return {
      id,
      project_id: projectId,
      branch: p?.branch ?? "main",
      commit,
      status,
      detail: "",
      job_id: job.id,
      image_id: "",
      image_tag: ok ? `golem-deploy/${p?.name}:${commit.slice(0, 12)}-b${id}` : "",
      engine_version: ENGINE,
      schema_fingerprint: "",
      webtransport_path: ok ? "/wt" : "",
      websocket_path: ok ? "/api/ws" : "",
      server_ids: [],
      created_by: null,
      created_at: iso(ageMs),
      started_at: iso(ageMs - 1000),
      finished_at: iso(ageMs - 4 * 60_000),
      ...extra,
    };
  };
  const okLog = (name: string) => [
    "# generated Dockerfile",
    "FROM golang:1.26-bookworm AS build",
    `#12 [build 6/9] RUN go run github.com/demiurgos-hub/golem-engine/cmd/golem-bake@${ENGINE}`,
    "#14 [build 8/9] RUN CGO_ENABLED=0 go build -trimpath -o /out/golem-server ./cmd/server",
    `#18 naming to docker.io/golem-deploy/${name} done`,
    "build succeeded",
  ];
  const builds: Build[] = [
    build(1, 1, "success", 9 * DAY, { commit: COMMIT_ARENA_OLD, image_id: IMG_ARENA_OLD, schema_fingerprint: FINGERPRINT_OLD }, okLog("arena-shooter")),
    build(2, 1, "error", 3 * DAY, { detail: "docker build failed; see the build log" }, [
      "#14 [build 8/9] RUN CGO_ENABLED=0 go build -trimpath ./cmd/server",
      "#14 0.912 cmd/server/main.go:41:2: undefined: host.Run",
      "error: docker build failed",
    ]),
    build(3, 1, "success", 2 * HOUR, { commit: COMMIT_ARENA, image_id: IMG_ARENA, schema_fingerprint: FINGERPRINT }, okLog("arena-shooter")),
    build(4, 2, "success", 3 * DAY, { commit: COMMIT_KART, image_id: IMG_KART }, okLog("kart-racer")),
    build(
      5,
      2,
      "error",
      1 * DAY,
      { detail: "go.mod requires github.com/demiurgos-hub/golem-engine v0.3.2; Golem Deploy needs v0.4.0-rc.1 or newer", engine_version: "v0.3.2" },
      [
        "preflight: reading golem-deploy.yaml, golem.yaml, go.mod",
        "error: go.mod requires github.com/demiurgos-hub/golem-engine v0.3.2; Golem Deploy needs v0.4.0-rc.1 or newer",
      ],
    ),
  ];

  const server = (id: number, name: string, port: number, desired: Server["desired_state"], state: Server["state"], extra: Partial<Server> = {}): Server => ({
    id,
    name,
    build_id: 3,
    project_id: 1,
    image: IMG_ARENA,
    commit: COMMIT_ARENA,
    engine_version: ENGINE,
    port,
    public_url: `https://${gameHost()}:${port}/`,
    allowed_origins: [],
    env_keys: ["ADMIN_TOKEN", "MAX_PLAYERS"],
    desired_state: desired,
    state,
    container_id: state === "missing" ? "" : `c0ffee${id}a1b2c3d4e5f60718293a4b5c6d7e8f90`,
    started_at: state === "running" ? iso(5 * HOUR) : null,
    exit_code: state === "exited" ? 0 : null,
    restart_required: false,
    certificate: null,
    // A server gets its data volume on its first start.
    data: {
      volume: state === "missing" ? "" : `golem-deploy-data-${id}-1`,
      schema_fingerprint: state === "missing" ? "" : FINGERPRINT,
      schema_mismatch: false,
      last_backup_at: null,
    },
    snapshot_interval_seconds: 300,
    backup_schedule: { interval_hours: 0, keep: 7 },
    inspector: false,
    inspector_active: false,
    created_at: iso(10 * DAY),
    updated_at: iso(6 * HOUR),
    ...extra,
  });
  const kart = { build_id: 4, project_id: 2, image: IMG_KART, commit: COMMIT_KART };
  const servers: Server[] = [
    // Connected to the Inspector.
    server(11, "eu-arena", 7000, "running", "running", {
      allowed_origins: ["https://play.example.com"], backup_schedule: { interval_hours: 24, keep: 7 }, inspector: true, inspector_active: true,
    }),
    // Runs the old build, but its world was saved by the newer one: it cannot start (schema mismatch).
    server(12, "staging", 7001, "stopped", "exited", { build_id: 1, image: IMG_ARENA_OLD, commit: COMMIT_ARENA_OLD }),
    server(13, "crashy", 7002, "running", "exited", { exit_code: 137, updated_at: iso(HOUR) }),
    // Its game's admin endpoint never answers: the runtime view answers 503. Started with the Inspector, its game never connects.
    server(14, "flood-test", 7003, "running", "running", { env_keys: [], snapshot_interval_seconds: 0, inspector: true, inspector_active: true }),
    server(15, "kart-eu", 7004, "running", "running", { ...kart, restart_required: true, snapshot_interval_seconds: 60, backup_schedule: { interval_hours: 6, keep: 3 } }),
    server(16, "fresh", 7005, "stopped", "missing", { env_keys: [] }),
    // Has a container, but "Docker" cannot be queried for it: every action fails, delete answers 503.
    server(17, "docker-lost", 7006, "running", "unknown", { env_keys: [], started_at: null }),
    // Created before servers ran builds: cannot start until a build is assigned.
    server(18, "legacy", 7007, "stopped", "missing", { build_id: null, project_id: null, image: "", commit: "", engine_version: "", env_keys: [] }),
  ];
  for (const s of servers) if (s.id === 15 || s.id === 14) s.data.schema_fingerprint = "";
  const backup = (id: number, s: Pick<Server, "id" | "name"> | { id: null; name: string }, kind: BackupKind, ageMs: number, extra: Partial<Backup> = {}): Backup => ({
    id,
    kind,
    server_id: s.id,
    server_name: s.name,
    project_id: 1,
    project_name: "arena-shooter",
    build_id: 3,
    commit: COMMIT_ARENA,
    engine_version: ENGINE,
    schema_fingerprint: FINGERPRINT,
    live: true,
    note: "",
    size_bytes: 4_200_000 + id * 37_000,
    content_bytes: 11_800_000 + id * 91_000,
    file_count: 3,
    omitted_entries: 0,
    sha256: fakeFingerprint(`backup-${id}`),
    job_id: null,
    created_by: kind === "scheduled" ? null : 1,
    created_by_username: kind === "scheduled" ? null : "admin",
    created_at: iso(ageMs),
    ...extra,
  });
  const arena = { id: 11, name: "eu-arena" };
  const old = { build_id: 1, commit: COMMIT_ARENA_OLD, schema_fingerprint: FINGERPRINT_OLD };
  const backups: Backup[] = [
    // "flood-test" keeps a link and a pipe next to its world: its backups leave them out.
    backup(10, { id: 14, name: "flood-test" }, "manual", HOUR, { note: "before the load test", omitted_entries: 2, schema_fingerprint: "" }),
    backup(9, arena, "scheduled", 2 * HOUR),
    backup(8, { id: 15, name: "kart-eu" }, "scheduled", 4 * HOUR, {
      project_id: 2, project_name: "kart-racer", build_id: 4, commit: COMMIT_KART, schema_fingerprint: "", size_bytes: 1_100_000,
    }),
    backup(7, arena, "scheduled", 26 * HOUR),
    backup(6, arena, "manual", 3 * DAY, { note: "before the map update" }),
    // A world saved by build 1: what "staging" (which runs build 1) can load again.
    backup(5, { id: 12, name: "staging" }, "manual", 4 * DAY, { ...old, note: "last good staging world", live: false }),
    backup(4, arena, "pre_restore", 5 * DAY, old),
    // The server was deleted since; its backups stay.
    backup(3, { id: null, name: "old-arena" }, "manual", 8 * DAY, { ...old, note: "season 1 final" }),
    backup(2, { id: null, name: "old-arena" }, "scheduled", 9 * DAY, { ...old, build_id: null }),
  ];
  const volume = (name: string, s: Pick<Server, "id" | "name"> | null, ageMs: number, serverName = s?.name ?? ""): Volume => ({
    name,
    server_id: s?.id ?? null,
    server_name: serverName,
    current: false,
    created_at: iso(ageMs),
    in_use: false,
  });
  const volumes: Volume[] = [
    ...servers.filter((s) => s.data.volume).map((s) => volume(s.data.volume, s, 10 * DAY)),
    // Left over after an earlier restore of eu-arena, and from the deleted old-arena.
    volume("golem-deploy-data-11-0", arena, 12 * DAY),
    volume("golem-deploy-data-9-1", null, 20 * DAY, "old-arena"),
  ];
  const serverCerts = new Map<number, CertificateInfo>();
  for (const s of servers) {
    if (s.container_id) serverCerts.set(s.id, selfSignedCert(`server-${s.id}`, 2 * DAY));
  }
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
    build_ids: [],
    in_use: false,
  });
  const images: Image[] = [
    image(IMG_ARENA, [`golem-deploy/arena-shooter:${COMMIT_ARENA.slice(0, 12)}-b3`], 1, COMMIT_ARENA, 2 * HOUR, 187_400_000),
    image(IMG_ARENA_OLD, [`golem-deploy/arena-shooter:${COMMIT_ARENA_OLD.slice(0, 12)}-b1`], 1, COMMIT_ARENA_OLD, 9 * DAY, 185_100_000),
    image(IMG_KART, [`golem-deploy/kart-racer:${COMMIT_KART.slice(0, 12)}-b4`], 2, COMMIT_KART, 3 * DAY, 243_900_000),
    image(IMG_ORPHAN, ["golem-deploy/kart-racer:0123456789ab-b0"], 2, "0123456789abcdef0123456789abcdef01234567", 4 * DAY, 12_000_000),
  ];
  const statics = new Map<string, { meta: StaticFile; blob: Blob }>();
  for (const [name, text, size] of [
    ["maps-pack-v3.zip", "PK\u0003\u0004pretend zip", 48_300_000],
    ["textures.zip", "PK\u0003\u0004pretend zip", 12_500_000],
    ["sound_bank.bin", "pretend audio", 9_100_000],
  ] as const) {
    statics.set(name, { meta: { name, size_bytes: size, modified_at: iso(5 * DAY), is_zip: name.endsWith(".zip") }, blob: new Blob([text]) });
  }
  return {
    projects,
    builds,
    servers,
    serverCerts,
    certMode: "self_signed",
    customCert: null,
    panelMode: "self_signed",
    panelSelfSigned: selfSignedCert("panel"),
    acme: {
      email: "", staging: false, terms_agreed: false, directory_url: ACME_PRODUCTION,
      http01_available: true, tls_alpn01_available: false, certificates: [],
    },
    env,
    images,
    statics,
    volumes,
    backups,
    restoreReads: new Map(),
    omittedEntries: new Map([[14, 2]]),
    inspector: {
      enabled: true, state: "running", detail: "", image: INSPECTOR_IMAGE, imageId: INSPECTOR_IMAGE_ID, startedAt: iso(3 * HOUR), secrets: 1,
      imagePresent: true, connected: new Set(), left: new Map(),
    },
    inspectorTokens: new Map(servers.filter((s) => s.inspector_active).map((s) => [s.id, 1])),
    restartPending: new Set(servers.filter((s) => s.restart_required).map((s) => s.id)),
    inspectorLinks: new Map(),
    jobs: jobs.reverse(),
    jobLogs,
    follows: new Map(),
    uploads: new Map(),
    nextId: 100,
    nextBuildId: 6,
    nextJobId,
    nextBackupId: 11,
    volumeSeq: 2,
    requestSeq: 0,
  };
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

function activeJobFor(st: MockState, type: SubjectType, id: number | string): Job | undefined {
  return st.jobs.find((j) => j.subject_type === type && j.subject_id === String(id) && isActive(j));
}

/** Starts a fake job that logs a few lines per step and then runs `finish`. A step "FAIL" makes it fail with `failDetail`. */
function startJob(
  st: MockState,
  me: Me,
  restricted: boolean,
  kind: JobKind,
  type: SubjectType,
  id: number | string,
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
      st.onJobEnd?.();
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

/** The server fields derived from its build (docs/API.md, Servers). */
function applyBuild(s: Server, b: Build | null) {
  s.build_id = b?.id ?? null;
  s.project_id = b?.project_id ?? null;
  s.image = b?.image_id ?? "";
  s.commit = b?.commit ?? "";
  s.engine_version = b?.engine_version ?? "";
}

/** Like the daemon's build lookup for servers: field `build_id`, successful builds only. */
function buildError(st: MockState, id: number | undefined): { message: string } | { build: Build } {
  if (id === undefined || id === null || id === 0) return { message: "build_id is required (the id of a successful build)" };
  const b = st.builds.find((x) => x.id === id);
  if (!b) return { message: `build ${id} does not exist` };
  if (b.status !== "success") return { message: `build ${id} has not succeeded; servers run successful builds only` };
  return { build: b };
}

/** Allowed origins: http(s)://host[:port], normalised; errors name the entry (allowed_origins[i]). */
function originsError(origins: unknown, field: string): { field: string; message: string } | { value: string[] } {
  if (origins === undefined || origins === null) return { value: [] };
  if (!Array.isArray(origins)) return { field, message: "allowed_origins must be a list" };
  if (origins.length > MAX_ORIGINS) return { field, message: `at most ${MAX_ORIGINS} allowed origins` };
  const out: string[] = [];
  for (const [i, raw] of origins.entries()) {
    const f = `${field}[${i}]`;
    if (typeof raw !== "string") return { field: f, message: "origin must be a string" };
    let u: URL;
    try {
      u = new URL(raw);
    } catch {
      return { field: f, message: `origin ${raw} is not a URL` };
    }
    if (u.protocol !== "http:" && u.protocol !== "https:") return { field: f, message: `origin ${raw} must use http or https` };
    if (u.username || u.password) return { field: f, message: `origin ${raw} must not contain credentials` };
    if ((u.pathname !== "/" && u.pathname !== "") || u.search || u.hash || /\/./.test(raw.replace(/^https?:\/\//, ""))) {
      return { field: f, message: `origin ${raw} must not have a path, query or fragment` };
    }
    const norm = `${u.protocol}//${u.host}`.toLowerCase();
    if (!out.includes(norm)) out.push(norm);
  }
  return { value: out };
}

const ADDON_TARGET_RE = /^(?!-)[A-Za-z0-9._-]+(\/(?!-)[A-Za-z0-9._-]+)*$/;
const ADDON_TARGET_MESSAGE = "target must be a relative path below the game root of letters, digits, '.', '_', '-' and '/' (no '..', no .git)";

/** Normalises a static add-on target like the daemon ("./assets/" becomes "assets", "" and "." the root). */
function normaliseTarget(raw: string): string | null {
  let t = raw.trim().replace(/^\.\/+/, "").replace(/\/+$/, "");
  if (t === "" || t === ".") return ".";
  t = t.replace(/\/+/g, "/");
  if (!ADDON_TARGET_RE.test(t)) return null;
  if (t.split("/").some((seg) => seg === "." || seg === ".." || seg === ".git")) return null;
  return t;
}

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

/** Like the daemon's checks of the autosave period and backup schedule; `prefix` is "" or "servers[i].". */
function dataSettingsError(req: { snapshot_interval_seconds?: number; backup_schedule?: BackupSchedule }, prefix = ""): { field: string; message: string } | null {
  const snap = req.snapshot_interval_seconds;
  if (snap !== undefined && snap !== null && (!Number.isInteger(snap) || (snap !== 0 && (snap < 10 || snap > 86_400)))) {
    return { field: `${prefix}snapshot_interval_seconds`, message: "snapshot_interval_seconds must be 0 or 10 to 86400" };
  }
  const b = req.backup_schedule;
  if (b !== undefined && b !== null) {
    if (!Number.isInteger(b.interval_hours) || b.interval_hours < 0 || b.interval_hours > 720) {
      return { field: `${prefix}backup_schedule.interval_hours`, message: "backup_schedule.interval_hours must be 0 (off) or 1 to 720" };
    }
    if (!Number.isInteger(b.keep) || b.keep < 1 || b.keep > 100) {
      return { field: `${prefix}backup_schedule.keep`, message: "backup_schedule.keep must be 1 to 100" };
    }
  }
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
  st.onJobEnd = noteInspectorTargets;
  console.info(`${MOCK_MARKER}: VITE_API_MOCK=1, projects/builds/servers/backups/images/static/volumes/certificates/Inspector are simulated in the browser.`);

  /** The caller, as the real daemon sees the bearer token; null lets the request through to the daemon (which answers 401). */
  async function identify(init: RequestInit | { headers: Record<string, string> }): Promise<Me | null> {
    const auth = (init.headers as Record<string, string> | undefined)?.Authorization;
    if (!auth) return null;
    const cached = identities.get(auth);
    if (cached) return cached;
    const res = await real.fetch(`${BASE}/account`, { headers: { Authorization: auth, Accept: "application/json" }, credentials: "omit" });
    if (!res.ok) return null;
    const me = (await res.json()) as CurrentAccount;
    const out: Me = { id: me.id, username: me.username, role: me.auth === "api_token" ? "developer" : me.role, auth: me.auth };
    identities.set(auth, out);
    return out;
  }

  const forbidden = () => fail(st, 403, "forbidden", "your role does not allow this action");

  /** Recomputes the cross references the daemon derives on read: builds' servers, images' builds and servers. */
  function refreshDerived() {
    for (const b of st.builds) b.server_ids = st.servers.filter((s) => s.build_id === b.id).map((s) => s.id);
    for (const img of st.images) {
      img.build_ids = st.builds.filter((b) => b.status === "success" && b.image_id === img.id).map((b) => b.id);
      img.server_ids = st.servers.filter((s) => s.image === img.id).map((s) => s.id);
      img.in_use = img.build_ids.length > 0 || img.server_ids.length > 0 || containerUsers(img.id).length > 0;
    }
    for (const s of st.servers) {
      s.certificate = st.certMode === "self_signed" ? (st.serverCerts.get(s.id) ?? null) : gameCertificate().certificate;
      const buildFp = st.builds.find((b) => b.id === s.build_id)?.schema_fingerprint ?? "";
      s.data.schema_mismatch = s.data.schema_fingerprint !== "" && buildFp !== "" && s.data.schema_fingerprint !== buildFp;
      s.data.last_backup_at = st.backups.find((b) => b.server_id === s.id)?.created_at ?? null;
    }
    for (const v of st.volumes) {
      const owner = st.servers.find((s) => s.id === v.server_id);
      if (!owner) v.server_id = null;
      v.current = owner !== undefined && owner.data.volume === v.name;
      // A server's container mounts its current data volume, also while stopped.
      v.in_use = v.current && owner !== undefined && owner.container_id !== "";
    }
  }

  /** A new data volume for the server (first start, restore, or a start after a reset). */
  function newVolume(s: Server) {
    const name = `golem-deploy-data-${s.id}-${++st.volumeSeq}`;
    st.volumes.push({ name, server_id: s.id, server_name: s.name, current: true, created_at: iso(), in_use: false });
    s.data.volume = name;
  }

  function dropVolume(name: string) {
    st.volumes = st.volumes.filter((v) => v.name !== name);
  }

  /** Stores a backup of the server's current data, as the jobs below do. */
  function addBackup(s: Server, kind: BackupKind, job: Job | null, me: Me | null, extra: Partial<Backup> = {}): Backup {
    const b = st.builds.find((x) => x.id === s.build_id);
    const id = st.nextBackupId++;
    const backup: Backup = {
      id,
      kind,
      server_id: s.id,
      server_name: s.name,
      project_id: s.project_id,
      project_name: st.projects.find((p) => p.id === s.project_id)?.name ?? "",
      build_id: s.build_id,
      commit: b?.commit ?? "",
      engine_version: b?.engine_version ?? "",
      schema_fingerprint: s.data.schema_fingerprint,
      live: s.state === "running",
      note: "",
      size_bytes: 3_000_000 + Math.round(Math.random() * 2_000_000),
      content_bytes: 9_000_000 + Math.round(Math.random() * 4_000_000),
      file_count: 3,
      omitted_entries: st.omittedEntries.get(s.id) ?? 0,
      sha256: fakeFingerprint(`backup-${id}-${Date.now()}`),
      job_id: job?.id ?? null,
      created_by: me?.id ?? null,
      created_by_username: me?.username ?? null,
      created_at: iso(),
      ...extra,
    };
    st.backups.unshift(backup);
    return backup;
  }

  /** Servers whose container was created from the image (a server keeps its container until its next start). */
  function containerUsers(imageId: string): number[] {
    return st.servers.filter((s) => s.container_id && (containerImage.get(s.id) ?? s.image) === imageId).map((s) => s.id);
  }
  /** The image each server's current container runs, when it differs from its configured build's image. */
  const containerImage = new Map<number, string>();

  function gameCertificate(): GameCertificate {
    return {
      mode: st.certMode, game_host: gameHost(),
      certificate: st.certMode === "custom" ? st.customCert : st.certMode === "acme" ? automaticCertificate() : null,
    };
  }

  function automaticCertificate() {
    return st.acme.certificates[0]?.certificate ?? null;
  }

  function panelCertificate(): PanelCertificate {
    const certificate = st.panelMode === "acme" ? automaticCertificate() : st.panelSelfSigned;
    return { mode: st.panelMode, configurable: true, hosts: [gameHost()], certificate, covers_hosts: Boolean(certificate) };
  }

  function refreshManagedCertificates(reissue = false) {
    const uses: ACMECertificateUse[] = [];
    if (st.certMode === "acme") uses.push("game");
    if (st.panelMode === "acme") uses.push("panel");
    if (uses.length === 0) {
      st.acme.certificates = [];
      return;
    }
    const old = st.acme.certificates[0];
    const current = !reissue && old ? old : {
      identifier: gameHost(),
      status: "issued" as const,
      certificate: {
        ...selfSignedCert(`acme-${Date.now()}`),
        self_signed: false,
        issuer: st.acme.staging ? "CN=Example Staging CA" : "CN=Example ACME CA",
        not_after: iso(-90 * DAY),
      },
      last_error: "",
      last_attempt_at: iso(),
    };
    st.acme.certificates = [{ ...current, used_by: uses }];
  }

  function certificateJob(me: Me, kind: "certificate.issue" | "certificate.renew", publish: () => void) {
    if (activeJobFor(st, "certificate", "acme")) return fail(st, 409, "job_in_progress", "another certificate job is already running");
    if (!st.acme.terms_agreed) return fail(st, 409, "conflict", "accept the ACME subscriber agreement first");
    return startJob(st, me, true, kind, "certificate", "acme", ["requesting a certificate", "answering HTTP-01 challenge"], () => {
      publish();
      refreshManagedCertificates(true);
      refreshDerived();
    });
  }

  function markRestartRequired() {
    for (const s of st.servers) {
      if (s.state !== "running") continue;
      st.restartPending.add(s.id);
      s.restart_required = true;
    }
  }

  /**
   * The Inspector secrets a new container of the server would get (null: no
   * Inspector variables). Like the daemon, only while its switch and the
   * installation's Inspector are on (game servers can always reach the mock's).
   */
  function inspectorWanted(s: Server): number | null {
    return s.inspector && st.inspector.enabled ? st.inspector.secrets : null;
  }

  /**
   * restart_required of a running server, like the daemon's config hash: its
   * other pending changes, or Inspector variables that differ from the ones
   * its container was started with. Turning a switch back before a restart
   * clears it again.
   */
  function syncRestartRequired(s: Server) {
    if (s.state !== "running") return;
    s.restart_required = st.restartPending.has(s.id) || (st.inspectorTokens.get(s.id) ?? null) !== inspectorWanted(s);
  }

  /** A new container of the server: it runs with the Inspector when its switch and the installation's Inspector are on. */
  function containerStarted(s: Server) {
    st.restartPending.delete(s.id);
    const want = inspectorWanted(s);
    s.inspector_active = want !== null;
    if (want !== null) st.inspectorTokens.set(s.id, want);
    else st.inspectorTokens.delete(s.id);
  }

  /** The server has no container any more. */
  function containerGone(s: Server) {
    s.inspector_active = false;
    st.inspectorTokens.delete(s.id);
  }

  /**
   * The games connected to the hub now: running servers started with the Inspector and its current secrets whose switch is still on (the
   * daemon closes the relay of a server whose switch was turned off and refuses it afterwards).
   */
  function hubConnected(): Server[] {
    const ins = st.inspector;
    if (ins.state !== "running") return [];
    return st.servers.filter(
      (s) =>
        s.inspector &&
        s.inspector_active &&
        s.state === "running" &&
        st.inspectorTokens.get(s.id) === ins.secrets &&
        !NO_INSPECTOR_SUPPORT.has(s.name),
    );
  }

  /** Notes games that left the hub (before every request and after every job): the hub lists them a while longer. */
  function noteInspectorTargets() {
    const ins = st.inspector;
    const now = new Set(hubConnected().map((s) => s.name));
    for (const name of ins.connected) if (!now.has(name)) ins.left.set(name, Date.now());
    for (const name of now) ins.left.delete(name);
    ins.connected = now;
  }

  /**
   * GET /inspector, like the daemon: everything but the setting is left out
   * while the Inspector is turned off (also while the job that turns it off
   * runs). The hub knows its targets by name (the server name); they are
   * matched to servers with the switch on, and the rest are other_targets.
   */
  function inspectorView(): Inspector {
    const ins = st.inspector;
    const base = { available: true, enabled: ins.enabled, image: ins.image, port: INSPECTOR_PORT, public_url: "", agents_reachable: true };
    if (!ins.enabled) {
      return { ...base, state: "off", detail: "", image_id: "", started_at: null, agent_address: "", targets: null, other_targets: 0 };
    }
    const running = ins.state === "running";
    let targets: InspectorTarget[] | null = null;
    let other = 0;
    if (running) {
      const hubStart = ins.startedAt ? Date.parse(ins.startedAt) : 0;
      const list = hubConnected().map((s) => ({
        name: s.name,
        connectedAt: new Date(Math.max(hubStart, s.started_at ? Date.parse(s.started_at) : 0) + 2000).toISOString() as string | null,
      }));
      for (const [name, at] of ins.left) {
        if (Date.now() - at < INSPECTOR_TARGET_LINGER_MS) list.push({ name, connectedAt: null });
        else ins.left.delete(name);
      }
      targets = [];
      for (const t of list) {
        const s = st.servers.find((x) => x.inspector && x.name === t.name);
        if (s) targets.push({ server_id: s.id, connected: t.connectedAt !== null, entities: 0, connected_at: t.connectedAt });
        else other++;
      }
    }
    return {
      ...base,
      state: ins.state,
      detail: ins.state === "stopped" ? ins.detail : "",
      // The image the hub container was created from and when it started: whenever the container runs, whatever the state.
      image_id: ins.imageId,
      started_at: ins.startedAt,
      agent_address: INSPECTOR_AGENT_ADDRESS,
      targets,
      other_targets: other,
    };
  }

  /** PATCH /inspector: the setting changes at once; a restricted inspector.apply job brings the Inspector in line. */
  function inspectorApply(me: Me, req: UpdateInspectorRequest): Response {
    for (const k of ["enabled", "pull_image", "rotate_secrets"] as const) {
      if (req[k] !== undefined && typeof req[k] !== "boolean") return fail(st, 400, "bad_request", `${k} must be true or false`, k);
    }
    const ins = st.inspector;
    const on = req.enabled ?? ins.enabled;
    if (!on && (req.pull_image || req.rotate_secrets)) {
      return fail(st, 400, "validation_failed", "pull_image and rotate_secrets need the Inspector to be on", req.pull_image ? "pull_image" : "rotate_secrets");
    }
    if (activeJobFor(st, "inspector", "hub")) return fail(st, 409, "job_in_progress", "another Inspector job is already running");
    ins.enabled = on;
    // Off takes effect at once, like the daemon's: unused links end before the job runs.
    if (!on) st.inspectorLinks.clear();
    // The setting counts at once: servers with the switch on now differ from (or match again) what their container was started with.
    for (const s of st.servers) syncRestartRequired(s);
    if (on && ins.state !== "running") {
      ins.state = "stopped";
      ins.detail = "the Inspector is being applied";
    }
    const download = req.pull_image === true || !ins.imagePresent;
    const steps = on
      ? [
          ...(req.rotate_secrets ? ["replacing the Inspector's secrets and the servers' Inspector tokens"] : []),
          download ? `downloading the Inspector image ${ins.image}` : `using the Inspector image ${ins.image}`,
          "creating container golem-deploy-inspector",
          "waiting for the Inspector to report ready",
          "the daemon is logged in at the hub",
          `the Inspector is running on port ${INSPECTOR_PORT}`,
        ]
      : ["the Inspector listener is closed; Inspector sessions and agent connections ended", "removing the Inspector hub container", "the Inspector is off"];
    const job = startJob(st, me, true, "inspector.apply", "inspector", "hub", steps, () => {
      // A new hub, or none: it knows no target yet.
      ins.connected = new Set();
      ins.left = new Map();
      if (!on) {
        Object.assign(ins, { state: "off", detail: "", startedAt: null, imageId: "" });
        return;
      }
      if (download) ins.imagePresent = true;
      if (req.rotate_secrets) {
        ins.secrets++;
        // Links issued under the old secrets end with them.
        st.inspectorLinks.clear();
      }
      Object.assign(ins, { state: "running", detail: "", startedAt: iso(), imageId: INSPECTOR_IMAGE_ID });
      // New secrets: containers started with the old ones cannot connect any more.
      for (const s of st.servers) syncRestartRequired(s);
    });
    return json(st, 202, job);
  }

  /** POST /inspector/sessions: a one-time entry link for a login session. */
  function inspectorSession(me: Me, req: CreateInspectorSessionRequest): Response {
    if (me.auth !== "session") return fail(st, 403, "session_required", "this action needs a login session; API tokens cannot open the Inspector");
    // Like the daemon: null is absent, a value that is no integer does not decode, and any other id is looked up (0 or less is unknown).
    const id: unknown = req.server_id ?? undefined;
    if (id !== undefined) {
      if (typeof id !== "number" || !Number.isSafeInteger(id)) return fail(st, 400, "bad_request", "server_id must be an integer");
      const s = st.servers.find((x) => x.id === id);
      if (!s) return fail(st, 404, "not_found", "server not found");
      if (!s.inspector) return fail(st, 400, "validation_failed", "the server's Inspector setting is off; turn it on and restart the server", "server_id");
    }
    // Refused as soon as the setting is off, also while the job that turns it off runs.
    if (!st.inspector.enabled || st.inspector.state !== "running") return fail(st, 409, "conflict", "the Inspector is not running");
    const now = Date.now();
    const links = (st.inspectorLinks.get(me.id) ?? []).filter((exp) => exp > now);
    st.inspectorLinks.set(me.id, links);
    if (links.length >= INSPECTOR_LINKS_PER_ACCOUNT) {
      return fail(st, 429, "rate_limited", "too many open Inspector links; use one or wait a minute", undefined, { "Retry-After": "60" });
    }
    const expires = now + INSPECTOR_LINK_MS;
    links.push(expires);
    // The link leads nowhere real: nothing listens on the mock's Inspector port.
    // Like the daemon's, the ticket (32 random bytes, base64url) is in the
    // fragment and the server stays bound to it, out of the link.
    const ticket = btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))))
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
    const session: InspectorSession = {
      path: `/_golem-deploy/enter#t=${ticket}`,
      port: INSPECTOR_PORT,
      public_url: "",
      expires_at: new Date(expires).toISOString(),
    };
    return json(st, 201, session);
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
      /^\/(projects|builds|servers|backups|images|static|volumes)(\/|$)/.test(path) ||
      /^\/certificates\/(game|panel|acme|acme\/renew)$/.test(path) ||
      /^\/inspector(\/sessions)?$/.test(path) ||
      (path === "/jobs" && ["project", "server", "certificate", "inspector"].includes(query.get("subject_type") ?? "")) ||
      (/^\/jobs\/\d+(\/log|\/cancel)?$/.test(path) && st.jobs.some((j) => path.split("/")[2] === String(j.id)));
    if (!mocked) return null;
    const me = await identify(init);
    if (!me) return null;
    const admin = me.role === "admin";
    noteInspectorTargets();

    // Jobs of mocked subjects (mock job ids start at 900001).
    if (method === "GET" && path === "/jobs") {
      const sid = query.get("subject_id");
      const limit = Number(query.get("limit") ?? 50);
      if (!Number.isInteger(limit) || limit < 1 || limit > 500) return fail(st, 400, "bad_request", "limit must be an integer from 1 to 500");
      return json(st, 200, { items: st.jobs.filter((j) => (admin || j.subject_type !== "certificate") && j.subject_type === query.get("subject_type") && (!sid || j.subject_id === sid)).slice(0, limit) });
    }
    if ((m = path.match(/^\/jobs\/(\d+)(\/log|\/cancel)?$/))) {
      const job = st.jobs.find((j) => j.id === Number(m?.[1]));
      if (!job) return fail(st, 404, "not_found", "job not found");
      if (!admin && job.subject_type === "certificate") return forbidden();
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

    // The Inspector: read by any identity, changed by admins, opened by login sessions.
    if (path === "/inspector") {
      if (method === "GET") return json(st, 200, inspectorView());
      if (method === "PATCH") {
        if (!admin) return forbidden();
        return inspectorApply(me, typeof init.body === "string" && init.body ? body<UpdateInspectorRequest>() : {});
      }
      return null;
    }
    if (path === "/inspector/sessions" && method === "POST") {
      return inspectorSession(me, typeof init.body === "string" && init.body ? body<CreateInspectorSessionRequest>() : {});
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
        static_addons: [],
        allow_custom_dockerfile: false,
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
    if ((m = path.match(/^\/projects\/(\d+)(\/pull|\/branch|\/branches|\/builds)?$/))) {
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
        let addons: StaticAddon[] | undefined;
        if (req.static_addons !== undefined) {
          if (!Array.isArray(req.static_addons)) return fail(st, 400, "bad_request", "static_addons must be a list", "static_addons");
          if (req.static_addons.length > MAX_ADDONS) return fail(st, 400, "validation_failed", `at most ${MAX_ADDONS} static add-ons are allowed`, "static_addons");
          addons = [];
          for (const [i, a] of req.static_addons.entries()) {
            const f = `static_addons[${i}]`;
            const target = normaliseTarget(a.target ?? "");
            if (target === null) return fail(st, 400, "validation_failed", ADDON_TARGET_MESSAGE, `${f}.target`);
            const file = st.statics.get(a.name ?? "");
            if (!file) return fail(st, 400, "validation_failed", "no static file has this name", `${f}.name`);
            if (!file.meta.is_zip) return fail(st, 400, "validation_failed", "the static file is not a ZIP archive", `${f}.name`);
            addons.push({ name: a.name, target });
          }
        }
        if (req.repo_url !== undefined) p.repo_url = req.repo_url;
        if (req.auth) {
          p.auth_type = req.auth.type;
          p.auth_username = req.auth.username ?? "";
          p.has_secret = req.auth.type !== "none";
        }
        if (addons) p.static_addons = addons;
        if (req.allow_custom_dockerfile !== undefined) p.allow_custom_dockerfile = req.allow_custom_dockerfile;
        p.updated_at = iso();
        return json(st, 200, p);
      }
      if (!sub && method === "DELETE") {
        if (!admin) return forbidden();
        if (activeJobFor(st, "project", p.id)) return fail(st, 409, "job_in_progress", "a job is running for this project; wait for it or cancel it");
        refreshDerived();
        const builds = st.builds.filter((b) => b.project_id === p.id);
        const configured = st.servers.filter((s) => s.build_id !== null && builds.some((b) => b.id === s.build_id)).map((s) => s.id);
        if (configured.length) {
          return fail(st, 409, "conflict", `servers are configured with builds of this project (server ids ${configured.join(", ")}); assign them another build or delete them first`);
        }
        const holders = [...new Set(builds.filter((b) => b.image_id).flatMap((b) => containerUsers(b.image_id)))];
        if (holders.length) {
          return fail(st, 409, "conflict", `the container of server ${holders.join(", ")} still uses this project's images (a server keeps its container, also when stopped, until it is started again or deleted); restart or delete those servers first`);
        }
        st.projects = st.projects.filter((x) => x.id !== p.id);
        const imageIds = new Set(builds.map((b) => b.image_id).filter(Boolean));
        st.builds = st.builds.filter((b) => b.project_id !== p.id);
        st.images = st.images.filter((i) => !imageIds.has(i.id));
        return json(st, 204, null);
      }
      if (sub === "/branches" && method === "GET") {
        if (p.repo_url.includes("unreachable")) return fail(st, 503, "service_unavailable", "the Git remote could not be reached");
        return json(st, 200, { current: p.branch, items: ["develop", "feature/netcode", "main"] });
      }
      if (sub === "/builds" && method === "GET") {
        const r = buildListFilter(query);
        if ("error" in r) return r.error;
        refreshDerived();
        return json(st, 200, { items: listBuilds({ ...r, projectId: p.id }) });
      }
      if (sub === "/builds" && method === "POST") return createBuild(p, typeof init.body === "string" && init.body ? body<CreateBuildRequest>() : {}, me);
      if ((sub === "/pull" || sub === "/branch") && method === "POST") {
        let branch = p.branch;
        if (sub === "/branch") {
          branch = body<SwitchBranchRequest>().branch ?? "";
          if (!branch) return fail(st, 400, "validation_failed", "branch is required", "branch");
        }
        if (activeJobFor(st, "project", p.id)) return fail(st, 409, "job_in_progress", "another job is already running for this project");
        return json(st, 202, startProjectUpdate(p, sub === "/pull" ? "pull" : "branch", branch, me));
      }
      return null;
    }

    // Builds.
    if (path === "/builds" && method === "GET") {
      const r = buildListFilter(query);
      if ("error" in r) return r.error;
      const pid = query.get("project_id");
      if (pid !== null && pid !== "" && !/^[1-9]\d*$/.test(pid)) return fail(st, 400, "bad_request", "project_id must be a positive integer");
      refreshDerived();
      return json(st, 200, { items: listBuilds({ ...r, projectId: pid ? Number(pid) : undefined }) });
    }
    if ((m = path.match(/^\/builds\/(\d+)$/))) {
      const b = st.builds.find((x) => x.id === Number(m?.[1]));
      if (!b) return fail(st, 404, "not_found", "build not found");
      refreshDerived();
      if (method === "GET") return json(st, 200, b);
      if (method === "DELETE") {
        if (!admin) return forbidden();
        if (b.status === "queued" || b.status === "running") return fail(st, 409, "conflict", "the build is still queued or running; cancel its job first");
        if (b.server_ids.length) return fail(st, 409, "conflict", "servers are configured with this build; assign them another build or delete them first");
        const holders = b.image_id ? containerUsers(b.image_id) : [];
        if (holders.length) {
          return fail(st, 409, "conflict", `the container of server ${holders.join(", ")} still uses this build's image (a server keeps its container, also when stopped, until it is started again or deleted); restart or delete those servers first`);
        }
        st.builds = st.builds.filter((x) => x.id !== b.id);
        if (b.image_id && !st.builds.some((x) => x.status === "success" && x.image_id === b.image_id)) st.images = st.images.filter((i) => i.id !== b.image_id);
        return json(st, 204, null);
      }
      return null;
    }

    // Servers.
    if (path === "/servers" && method === "GET") {
      refreshDerived();
      return json(st, 200, { items: st.servers });
    }
    if (path === "/servers" && method === "POST") {
      const req = body<CreateServerRequest>();
      if (!NAME_RE.test(req.name ?? "")) return fail(st, 400, "validation_failed", "name must be 1-63 characters of a-z, 0-9 and '-', starting with a letter or digit", "name");
      const be = buildError(st, req.build_id);
      if ("message" in be) return fail(st, 400, "validation_failed", be.message, "build_id");
      if (req.port !== undefined) {
        const pe = portError(req.port);
        if (pe) return fail(st, 400, "validation_failed", pe, "port");
      }
      const oe = originsError(req.allowed_origins, "allowed_origins");
      if ("message" in oe) return fail(st, 400, "validation_failed", oe.message, oe.field);
      const ee = envError(req.env, "env");
      if (ee) return fail(st, 400, "validation_failed", ee.message, ee.field);
      const de = dataSettingsError(req);
      if (de) return fail(st, 400, "validation_failed", de.message, de.field);
      if (req.inspector !== undefined && typeof req.inspector !== "boolean") return fail(st, 400, "bad_request", "inspector must be true or false", "inspector");
      if (st.servers.some((s) => s.name === req.name)) return fail(st, 409, "name_taken", "a server with that name already exists");
      const port = req.port ?? nextPort(st);
      if (port === null || st.servers.some((s) => s.port === port)) return fail(st, 409, "port_in_use", "that port is already assigned to another server (or the port range is full)");
      const s = newServer(req.name, port, oe.value, req.env, req.desired_state ?? "stopped");
      if (req.snapshot_interval_seconds !== undefined) s.snapshot_interval_seconds = req.snapshot_interval_seconds;
      if (req.backup_schedule) s.backup_schedule = { ...req.backup_schedule };
      if (req.inspector) s.inspector = true;
      applyBuild(s, be.build);
      st.servers.push(s);
      if (s.desired_state === "running") startServerAction(s, "start", me);
      refreshDerived();
      return json(st, 201, s);
    }
    if (path === "/servers/export" && method === "GET") {
      if (!admin) return forbidden();
      const out: ServerExport = {
        format_version: 2,
        servers: st.servers.map((s) => {
          const b = st.builds.find((x) => x.id === s.build_id);
          return {
            name: s.name,
            project: b ? (st.projects.find((p) => p.id === b.project_id)?.name ?? "") : "",
            ...(b?.commit ? { commit: b.commit } : {}),
            port: s.port,
            allowed_origins: s.allowed_origins,
            env: st.env.get(s.id) ?? {},
            desired_state: s.desired_state,
            snapshot_interval_seconds: s.snapshot_interval_seconds,
            backup_schedule: s.backup_schedule,
            ...(s.inspector ? { inspector: true } : {}),
          };
        }),
      };
      return json(st, 200, out);
    }
    if (path === "/servers/import" && method === "POST") {
      if (!admin) return forbidden();
      const req = body<ServerExport & { servers?: Record<string, unknown>[] }>();
      // Unknown fields are a decode error (400 bad_request), e.g. the image of a version 1 document.
      for (const spec of req.servers ?? []) {
        const extra = Object.keys(spec).find(
          (k) => !["name", "project", "commit", "port", "allowed_origins", "env", "desired_state", "snapshot_interval_seconds", "backup_schedule", "inspector"].includes(k),
        );
        if (extra) return fail(st, 400, "bad_request", `unknown field "${extra}"`, extra);
        // A JSON value that is not a boolean is a decode error too.
        if (spec.inspector !== undefined && typeof spec.inspector !== "boolean") return fail(st, 400, "bad_request", "inspector must be true or false", "inspector");
      }
      if (req.format_version !== 2) return fail(st, 400, "validation_failed", "format_version must be 2", "format_version");
      if (!req.servers?.length) return fail(st, 400, "validation_failed", "the document contains no servers", "servers");
      if (req.servers.length > MAX_IMPORT) return fail(st, 400, "validation_failed", `at most ${MAX_IMPORT} servers can be imported at once`, "servers");
      // Like the daemon: stop at the first problem, one error envelope, nothing created.
      const names = new Set<string>();
      const ports = new Set<number>();
      const resolved: { build: Build; origins: string[] }[] = [];
      for (const [i, spec] of req.servers.entries()) {
        const f = (n: string) => `servers[${i}].${n}`;
        if (!NAME_RE.test(spec.name ?? "")) return fail(st, 400, "validation_failed", "name must be 1-63 characters of a-z, 0-9 and '-', starting with a letter or digit", f("name"));
        if (names.has(spec.name)) return fail(st, 400, "validation_failed", `duplicate server name "${spec.name}" in the document`, f("name"));
        names.add(spec.name);
        if (st.servers.some((s) => s.name === spec.name)) return fail(st, 409, "name_taken", "a server with that name already exists");
        if (spec.desired_state && spec.desired_state !== "running" && spec.desired_state !== "stopped") return fail(st, 400, "validation_failed", 'desired_state must be "running" or "stopped"', f("desired_state"));
        const ee = envError(spec.env, f("env"));
        if (ee) return fail(st, 400, "validation_failed", ee.message, ee.field);
        const oe = originsError(spec.allowed_origins, f("allowed_origins"));
        if ("message" in oe) return fail(st, 400, "validation_failed", oe.message, oe.field);
        const de = dataSettingsError(spec, `servers[${i}].`);
        if (de) return fail(st, 400, "validation_failed", de.message, de.field);
        if (!spec.project) return fail(st, 400, "validation_failed", "project is required (the server runs a build of that project)", f("project"));
        if (spec.commit && !/^[0-9a-f]{40}([0-9a-f]{24})?$/.test(spec.commit)) return fail(st, 400, "validation_failed", "commit must be a full lower-case commit hash", f("commit"));
        const p = st.projects.find((x) => x.name === spec.project);
        if (!p) return fail(st, 400, "validation_failed", `project "${spec.project}" does not exist`, f("project"));
        const candidates = listBuilds({ projectId: p.id, status: "success" }).filter((b) => !spec.commit || b.commit === spec.commit);
        if (!candidates.length) {
          return spec.commit
            ? fail(st, 400, "validation_failed", `project "${spec.project}" has no successful build of commit ${spec.commit}`, f("commit"))
            : fail(st, 400, "validation_failed", `project "${spec.project}" has no successful build`, f("project"));
        }
        if (spec.port !== undefined) {
          const pe = portError(spec.port);
          if (pe) return fail(st, 400, "validation_failed", pe, f("port"));
          if (ports.has(spec.port)) return fail(st, 400, "validation_failed", `duplicate port ${spec.port} in the document`, f("port"));
          if (st.servers.some((s) => s.port === spec.port)) return fail(st, 409, "port_in_use", "that port is already assigned to another server (or the port range is full)");
          ports.add(spec.port);
        }
        resolved.push({ build: candidates[0], origins: oe.value });
      }
      const created = req.servers.map((spec, i) => {
        const s = newServer(spec.name, spec.port ?? nextPort(st) ?? 0, resolved[i].origins, spec.env, "stopped");
        if (spec.snapshot_interval_seconds !== undefined) s.snapshot_interval_seconds = spec.snapshot_interval_seconds;
        if (spec.backup_schedule) s.backup_schedule = { ...spec.backup_schedule };
        if (spec.inspector === true) s.inspector = true;
        applyBuild(s, resolved[i].build);
        st.servers.push(s);
        return s;
      });
      refreshDerived();
      return json(st, 201, { created });
    }
    if ((m = path.match(/^\/servers\/(\d+)(\/start|\/stop|\/restart|\/logs|\/stats|\/runtime|\/connectivity|\/backups|\/restore|\/data\/reset)?$/))) {
      const s = st.servers.find((x) => x.id === Number(m?.[1]));
      if (!s) return fail(st, 404, "not_found", "server not found");
      const sub = m[2];
      refreshDerived();
      if (!sub && method === "GET") return json(st, 200, s);
      if (sub === "/connectivity" && method === "GET") {
        if (s.state === "unknown") return fail(st, 503, "service_unavailable", "Docker is unreachable");
        const build = st.builds.find((b) => b.id === s.build_id);
        const running = s.state === "running";
        const out: ServerConnectivity = {
          running, page_url: s.public_url,
          webtransport_url: new URL(build?.webtransport_path || "/wt", s.public_url).href,
          websocket_url: new URL(build?.websocket_path || "/api/ws", s.public_url).href.replace(/^https:/, "wss:"),
          pinned: st.certMode === "self_signed", certificate: s.certificate,
          test_origins: [window.location.origin], test_origins_active: running && !s.restart_required,
        };
        return json(st, 200, out);
      }
      if (!sub && method === "PATCH") {
        const req = body<UpdateServerRequest>();
        for (const k of req.env_unset ?? []) {
          if (req.env_set && k in req.env_set) return fail(st, 400, "validation_failed", "a variable cannot be in both env_set and env_unset", "env_unset");
        }
        const ee = envError(req.env_set, "env_set");
        if (ee) return fail(st, 400, "validation_failed", ee.message, ee.field);
        const de = dataSettingsError(req);
        if (de) return fail(st, 400, "validation_failed", de.message, de.field);
        if (req.inspector !== undefined && typeof req.inspector !== "boolean") return fail(st, 400, "bad_request", "inspector must be true or false", "inspector");
        let build: Build | null = st.builds.find((b) => b.id === s.build_id) ?? null;
        if (req.build_id !== undefined) {
          const be = buildError(st, req.build_id);
          if ("message" in be) return fail(st, 400, "validation_failed", be.message, "build_id");
          // A server's project is fixed at creation (servers without a build may take any).
          if (s.project_id !== null && be.build.project_id !== s.project_id) {
            return fail(st, 400, "validation_failed", `build ${be.build.id} belongs to another project; a server only runs builds of its own project`, "build_id");
          }
          build = be.build;
        }
        let origins = s.allowed_origins;
        if (req.allowed_origins !== undefined) {
          const oe = originsError(req.allowed_origins, "allowed_origins");
          if ("message" in oe) return fail(st, 400, "validation_failed", oe.message, oe.field);
          origins = oe.value;
        }
        const portChange = req.port !== undefined && req.port !== s.port;
        const buildChange = req.build_id !== undefined && req.build_id !== s.build_id;
        if (req.port !== undefined) {
          const pe = portError(req.port);
          if (pe) return fail(st, 400, "validation_failed", pe, "port");
          if (st.servers.some((x) => x.id !== s.id && x.port === req.port)) return fail(st, 409, "port_in_use", "that port is already assigned to another server (or the port range is full)");
        }
        if ((portChange || buildChange) && activeJobFor(st, "server", s.id)) {
          return fail(st, 409, "job_in_progress", "a job is running for this server; wait for it to finish before changing its port or build");
        }
        if (portChange) {
          if (s.state === "unknown") return fail(st, 503, "service_unavailable", "Docker is unreachable");
          if (s.state === "running" || s.state === "restarting" || s.state === "paused") return fail(st, 409, "conflict", "stop the server before changing its port");
        }
        // A variable named RACE simulates a concurrent env change that keeps colliding.
        if (req.env_set && "RACE" in req.env_set) return fail(st, 409, "conflict", "the change conflicts with a concurrent change; retry");
        const env = { ...(st.env.get(s.id) ?? {}) };
        for (const k of req.env_unset ?? []) delete env[k];
        Object.assign(env, req.env_set ?? {});
        const originsChanged = origins.join(",") !== s.allowed_origins.join(",");
        // The autosave period is part of the container's definition; the backup schedule is not.
        const snapshotChange = req.snapshot_interval_seconds !== undefined && req.snapshot_interval_seconds !== s.snapshot_interval_seconds;
        // The Inspector switch counts on its own (syncRestartRequired): only while the installation's Inspector is on.
        const changed =
          buildChange || portChange || originsChanged || snapshotChange || Object.keys(req.env_set ?? {}).length > 0 || (req.env_unset ?? []).length > 0;
        if (req.snapshot_interval_seconds !== undefined) s.snapshot_interval_seconds = req.snapshot_interval_seconds;
        if (req.backup_schedule) s.backup_schedule = { ...req.backup_schedule };
        if (req.inspector !== undefined) s.inspector = req.inspector;
        st.env.set(s.id, env);
        // The current container keeps the old build's image until the next start.
        if (buildChange && s.container_id && !containerImage.has(s.id)) containerImage.set(s.id, s.image);
        applyBuild(s, build);
        s.allowed_origins = origins;
        if (req.port !== undefined) {
          s.port = req.port;
          s.public_url = `https://${gameHost()}:${req.port}/`;
        }
        s.env_keys = Object.keys(env).sort();
        s.updated_at = iso();
        // PATCH never restarts; a running container now runs an older definition.
        if (changed && s.state === "running") st.restartPending.add(s.id);
        syncRestartRequired(s);
        refreshDerived();
        return json(st, 200, s);
      }
      if (!sub && method === "DELETE") {
        // Present means exactly one of 1, true, 0 or false; an empty or repeated value is not a choice.
        const rawSkip = query.getAll("skip_safety_backup");
        if (rawSkip.length > 1 || (rawSkip.length === 1 && !["1", "true", "0", "false"].includes(rawSkip[0]))) {
          return fail(st, 400, "bad_request", "skip_safety_backup must be 1, true, 0 or false");
        }
        const skipSafety = rawSkip[0] === "1" || rawSkip[0] === "true";
        if (skipSafety && !admin) return fail(st, 403, "forbidden", "only an administrator can delete a server without its last backup");
        if (activeJobFor(st, "server", s.id)) return fail(st, 409, "job_in_progress", "another job is already running for this server");
        // A server.delete job, awaited like the daemon does; with Docker unreachable it fails and nothing is deleted.
        const lost = s.state === "unknown";
        // A last backup of the saved data comes first; one that would leave entries out is stored, then stops the deletion.
        const safety = !lost && !skipSafety && s.data.volume !== "";
        const omitted = safety ? (st.omittedEntries.get(s.id) ?? 0) : 0;
        const wasRunning = s.state === "running";
        const refusal = `the last backup of the saved data would leave ${omitted} ${omitted === 1 ? "entry" : "entries"} of the data directory out (links, pipes, devices or names that are not allowed); it was stored, but nothing was deleted. An administrator can delete the server anyway with skip_safety_backup=1`;
        const job: Job = startJob(
          st,
          me,
          false,
          "server.delete",
          "server",
          s.id,
          lost
            ? [`deleting server "${s.name}"`, "FAIL"]
            : [
                ...(wasRunning ? [`stopping container golem-deploy-${s.name}`] : []),
                ...(safety ? ["taking a last backup of the saved data"] : []),
                ...(omitted > 0 ? [...putBack(wasRunning, s), "FAIL"] : ["removing container"]),
              ],
          (ok) => {
            if (safety) addBackup(s, "pre_delete", job, me);
            if (!ok && wasRunning) putBackState(s);
          },
          lost ? "Docker is unreachable" : refusal,
        );
        while (isActive(job)) await sleep(200);
        // Only a delete job that ended in success deletes anything.
        if (lost) return fail(st, 503, "service_unavailable", "Docker is unreachable");
        if (omitted > 0) return fail(st, 409, "conflict", refusal);
        if (job.status !== "success") return fail(st, 409, "conflict", "the deletion was cancelled");
        st.servers = st.servers.filter((x) => x.id !== s.id);
        // Its data volume goes with it; its backups stay, without the server id.
        if (s.data.volume) dropVolume(s.data.volume);
        for (const b of st.backups) if (b.server_id === s.id) b.server_id = null;
        st.env.delete(s.id);
        st.serverCerts.delete(s.id);
        containerImage.delete(s.id);
        st.inspectorTokens.delete(s.id);
        return json(st, 204, null);
      }
      if ((sub === "/start" || sub === "/stop" || sub === "/restart") && method === "POST") {
        if (activeJobFor(st, "server", s.id)) return fail(st, 409, "job_in_progress", "another job is already running for this server");
        return json(st, 202, startServerAction(s, sub.slice(1) as "start" | "stop" | "restart", me));
      }
      if (sub === "/backups" && method === "POST") {
        const req = typeof init.body === "string" && init.body ? body<CreateBackupRequest>() : {};
        // Like servers.ValidateNote, after removing the surrounding space.
        const note = trimBackupNote(req.note ?? "");
        if ([...note].length > BACKUP_NOTE_MAX) return fail(st, 400, "validation_failed", `note must be at most ${BACKUP_NOTE_MAX} characters`, "note");
        if (BACKUP_NOTE_FORBIDDEN.test(note)) return fail(st, 400, "validation_failed", "note must be a single line without control characters", "note");
        if (activeJobFor(st, "server", s.id)) return fail(st, 409, "job_in_progress", "another job is already running for this server");
        return json(st, 202, backupJob(s, note, me));
      }
      if (sub === "/restore" && method === "POST") {
        if (!admin) return forbidden();
        const req = body<RestoreServerRequest>();
        const b = st.backups.find((x) => x.id === req.backup_id);
        if (!b) return fail(st, 400, "validation_failed", `backup ${req.backup_id} does not exist`, "backup_id");
        const build = st.builds.find((x) => x.id === s.build_id && x.status === "success");
        if (!build) return fail(st, 409, "conflict", "the server has no successful build; assign one before restoring a backup into it");
        if (activeJobFor(st, "server", s.id)) return fail(st, 409, "job_in_progress", "another job is already running for this server");
        if (!req.force && b.schema_fingerprint && build.schema_fingerprint && b.schema_fingerprint !== build.schema_fingerprint) {
          return fail(
            st,
            409,
            "schema_mismatch",
            `the backup was saved with schema ${b.schema_fingerprint.slice(0, 12)}, but build ${build.id} has schema ${build.schema_fingerprint.slice(0, 12)}; the server would not load it (restore with force to do it anyway)`,
          );
        }
        return json(st, 202, restoreJob(s, b, req.skip_safety_backup === true, me));
      }
      if (sub === "/data/reset" && method === "POST") {
        if (!admin) return forbidden();
        const req = typeof init.body === "string" && init.body ? body<ResetServerDataRequest>() : {};
        if (activeJobFor(st, "server", s.id)) return fail(st, 409, "job_in_progress", "another job is already running for this server");
        return json(st, 202, resetJob(s, req.skip_safety_backup === true, me));
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
      if ((sub === "/stats" || sub === "/runtime") && method === "GET") {
        if (s.state === "missing") return fail(st, 409, "conflict", "the server has no container yet; start it first");
        if (s.state === "unknown") return fail(st, 503, "service_unavailable", "Docker is unreachable");
        if (s.state !== "running") return fail(st, 409, "conflict", "the server is not running");
        if (sub === "/runtime") return runtime(s);
        return json(st, 200, {
          cpu_percent: 5 + Math.random() * 40,
          memory_bytes: Math.round(300_000_000 + Math.random() * 50_000_000),
          memory_limit_bytes: 2048 * 1024 * 1024,
          network_rx_bytes: Math.round((Date.now() / 10) % 1e9),
          network_tx_bytes: Math.round((Date.now() / 7) % 1e9),
          pids: 23,
          sampled_at: iso(),
        });
      }
      return null;
    }

    // The game certificate (admin).
    if (path === "/certificates/game") {
      if (!admin) return forbidden();
      if (method === "GET") return json(st, 200, gameCertificate());
      if (method === "PUT") {
        const req = body<UpdateGameCertificateRequest>();
        if (activeJobFor(st, "certificate", "acme")) return fail(st, 409, "job_in_progress", "another certificate job is already running");
        if (req.mode === "acme") {
          if (req.certificate_pem || req.private_key_pem) {
            return fail(st, 400, "validation_failed", "certificate_pem and private_key_pem are only accepted with mode custom", "certificate_pem");
          }
          const job = certificateJob(me, "certificate.issue", () => {
            if (st.certMode === "self_signed") markRestartRequired();
            st.certMode = "acme";
          });
          return job instanceof Response ? job : json(st, 202, { certificate: gameCertificate(), job });
        }
        if (req.mode === "self_signed") {
          if (req.certificate_pem || req.private_key_pem) {
            return fail(st, 400, "validation_failed", "certificate_pem and private_key_pem are only accepted with mode custom", "certificate_pem");
          }
          if (st.certMode !== "self_signed") markRestartRequired();
          st.certMode = "self_signed";
          // A new self-signed certificate for each server, picked up without a restart.
          for (const id of st.serverCerts.keys()) st.serverCerts.set(id, selfSignedCert(`server-${id}-${Date.now()}`));
        } else if (req.mode === "custom") {
          const chain = req.certificate_pem ?? "";
          const key = req.private_key_pem ?? "";
          if (!chain.includes("-----BEGIN CERTIFICATE-----")) return fail(st, 400, "validation_failed", "certificate_pem contains no PEM CERTIFICATE block", "certificate_pem");
          if (chain.includes("EXPIRED")) return fail(st, 400, "validation_failed", "the certificate is not currently valid (it expired)", "certificate_pem");
          if (!/-----BEGIN (RSA |EC )?PRIVATE KEY-----/.test(key)) return fail(st, 400, "validation_failed", "private_key_pem contains no unencrypted PEM private key", "private_key_pem");
          if (key.includes("MISMATCH")) return fail(st, 400, "validation_failed", "the private key does not match the certificate", "private_key_pem");
          if (st.certMode === "self_signed") markRestartRequired();
          st.certMode = "custom";
          const names = ["play.example.com", ...(chain.includes("COVERS") ? [gameHost()] : [])];
          st.customCert = {
            subject: "CN=play.example.com,O=Example Games",
            issuer: "CN=Example Issuing CA,O=Example Trust",
            dns_names: names,
            ip_addresses: [],
            not_before: iso(10 * DAY),
            not_after: iso(-80 * DAY),
            fingerprint_sha256: fakeFingerprint(chain),
            self_signed: false,
            chain_length: (chain.match(/-----BEGIN CERTIFICATE-----/g) ?? []).length,
            covers_game_host: names.includes(gameHost()),
          };
        } else {
          return fail(st, 400, "validation_failed", 'mode must be "self_signed", "custom" or "acme"', "mode");
        }
        refreshManagedCertificates();
        refreshDerived();
        return json(st, 200, { certificate: gameCertificate(), job: null });
      }
      return null;
    }

    if (path === "/certificates/panel") {
      if (!admin) return forbidden();
      if (method === "GET") return json(st, 200, panelCertificate());
      if (method === "PUT") {
        const req = body<UpdatePanelCertificateRequest>();
        if (activeJobFor(st, "certificate", "acme")) return fail(st, 409, "job_in_progress", "another certificate job is already running");
        if (req.mode === "acme") {
          const job = certificateJob(me, "certificate.issue", () => { st.panelMode = "acme"; });
          return job instanceof Response ? job : json(st, 202, { certificate: panelCertificate(), job });
        }
        if (req.mode !== "self_signed") return fail(st, 400, "validation_failed", 'mode must be "self_signed" or "acme"', "mode");
        st.panelMode = "self_signed";
        st.panelSelfSigned = selfSignedCert(`panel-${Date.now()}`);
        refreshManagedCertificates();
        return json(st, 200, { certificate: panelCertificate(), job: null });
      }
    }
    if (path === "/certificates/acme") {
      if (!admin) return forbidden();
      if (method === "GET") return json(st, 200, st.acme);
      if (method === "PUT") {
        const req = body<UpdateACMERequest>();
        if (req.staging !== undefined && req.staging !== st.acme.staging && st.acme.certificates.length) {
          return fail(st, 409, "conflict", "switch away from automatic certificates before changing staging");
        }
        if (req.terms_agreed === false && st.acme.certificates.length) {
          return fail(st, 409, "conflict", "switch both game and panel away from ACME before clearing agreement to the subscriber terms");
        }
        if (req.email && (new TextEncoder().encode(req.email).length > 254 || !/^[^\s@]+@[^\s@]+$/.test(req.email))) {
          return fail(st, 400, "validation_failed", "email must be a single address", "email");
        }
        Object.assign(st.acme, req);
        st.acme.directory_url = st.acme.staging ? ACME_STAGING : ACME_PRODUCTION;
        return json(st, 200, st.acme);
      }
    }
    if (path === "/certificates/acme/renew" && method === "POST") {
      if (!admin) return forbidden();
      if (!st.acme.certificates.length) return fail(st, 409, "conflict", "there are no managed certificates to renew");
      const job = certificateJob(me, "certificate.renew", () => {});
      return job instanceof Response ? job : json(st, 202, job);
    }

    // Images, static files, volumes.
    if (path === "/images" && method === "GET") {
      refreshDerived();
      return json(st, 200, { items: st.images });
    }
    if ((m = path.match(/^\/images\/(.+)$/)) && method === "DELETE") {
      if (!admin) return forbidden();
      refreshDerived();
      const id = decodeURIComponent(m[1]);
      const img = st.images.find((i) => i.id === id);
      if (!img) return fail(st, 404, "not_found", "image not found");
      if (img.build_ids.length) return fail(st, 409, "conflict", `the image belongs to build ${img.build_ids.join(", ")}; delete the build instead (DELETE /api/v1/builds/{id})`);
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
        const users = addonUsers(name);
        if (users.length) return fail(st, 409, "conflict", `the file is a static add-on of project ${users.join(", ")}; remove it from the project first`);
        st.statics.delete(name);
        return json(st, 204, null);
      }
      return null;
    }
    if (path === "/volumes" && method === "GET") {
      refreshDerived();
      return json(st, 200, { items: st.volumes });
    }
    if ((m = path.match(/^\/volumes\/([^/]+)$/)) && method === "DELETE") {
      if (!admin) return forbidden();
      refreshDerived();
      const v = st.volumes.find((x) => x.name === decodeURIComponent(m?.[1] ?? ""));
      if (!v) return fail(st, 404, "not_found", "volume not found");
      if (v.in_use) return fail(st, 409, "conflict", "the volume is in use by a container");
      if (v.current) return fail(st, 409, "conflict", `the volume holds the current data of server ${v.server_name}; delete the server or reset its data instead`);
      dropVolume(v.name);
      return json(st, 204, null);
    }

    // Backups.
    if (path === "/backups" && method === "GET") {
      const sid = query.get("server_id");
      if (sid !== null && sid !== "" && !/^[1-9]\d*$/.test(sid)) return fail(st, 400, "bad_request", "server_id must be a positive integer");
      const items = st.backups.filter((b) => !sid || b.server_id === Number(sid));
      return json(st, 200, { items, total_bytes: st.backups.reduce((n, b) => n + b.size_bytes, 0), max_content_bytes: BACKUP_MAX_CONTENT });
    }
    if ((m = path.match(/^\/backups\/(\d+)(\/download)?$/))) {
      const b = st.backups.find((x) => x.id === Number(m?.[1]));
      if (!b) return fail(st, 404, "not_found", "backup not found");
      if (method === "GET" && !m[2]) return json(st, 200, b);
      if (method === "GET" && m[2]) {
        // Not a real archive: the gzip magic and a line of text.
        const bytes = new Uint8Array([0x1f, 0x8b, 0x08, 0x00, ...new TextEncoder().encode(` pretend backup ${b.id} of ${b.server_name}`)]);
        return new Response(new Blob([bytes]), {
          status: 200,
          headers: headers(st, { "Content-Type": "application/gzip", "Content-Disposition": `attachment; filename="${b.server_name || "server"}-backup-${b.id}.tar.gz"` }),
        });
      }
      if (method === "DELETE") {
        if (!admin) return forbidden();
        // Only a restore that is running reads the archive; a queued one checks it again when it starts.
        if ([...st.restoreReads].some(([jobId, backupId]) => backupId === b.id && st.jobs.some((j) => j.id === jobId && j.status === "running"))) {
          return fail(st, 409, "conflict", "a restore is reading this backup; wait for it to finish");
        }
        st.backups = st.backups.filter((x) => x.id !== b.id);
        return json(st, 204, null);
      }
      return null;
    }
    return null;
  }

  /** Names of the projects that use a static file as an add-on. */
  function addonUsers(name: string): string[] {
    return st.projects.filter((p) => p.static_addons.some((a) => a.name === name)).map((p) => p.name);
  }

  function newServer(name: string, port: number, origins: string[], env: Record<string, string> | undefined, desired: Server["desired_state"]): Server {
    const s: Server = {
      id: st.nextId++,
      name,
      build_id: null,
      project_id: null,
      image: "",
      commit: "",
      engine_version: "",
      port,
      public_url: `https://${gameHost()}:${port}/`,
      allowed_origins: origins,
      env_keys: Object.keys(env ?? {}).sort(),
      desired_state: desired,
      state: "missing",
      container_id: "",
      started_at: null,
      exit_code: null,
      restart_required: false,
      certificate: null,
      data: { volume: "", schema_fingerprint: "", schema_mismatch: false, last_backup_at: null },
      snapshot_interval_seconds: 300,
      backup_schedule: { interval_hours: 0, keep: 7 },
      inspector: false,
      inspector_active: false,
      created_at: iso(),
      updated_at: iso(),
    };
    st.env.set(s.id, { ...(env ?? {}) });
    return s;
  }

  function buildListFilter(query: URLSearchParams): { status?: string; limit: number } | { error: Response } {
    const status = query.get("status") ?? "";
    if (status && !["queued", "running", "success", "error", "cancelled"].includes(status)) {
      return { error: fail(st, 400, "bad_request", "status must be queued, running, success, error or cancelled") };
    }
    const rawLimit = query.get("limit");
    const limit = rawLimit === null || rawLimit === "" ? 50 : Number(rawLimit);
    if (!Number.isInteger(limit) || limit < 1 || limit > 500) return { error: fail(st, 400, "bad_request", "limit must be an integer from 1 to 500") };
    return { status: status || undefined, limit };
  }

  function listBuilds(f: { projectId?: number; status?: string; limit?: number }): Build[] {
    return st.builds
      .filter((b) => (f.projectId === undefined || b.project_id === f.projectId) && (!f.status || b.status === f.status))
      .sort((a, b) => b.id - a.id)
      .slice(0, f.limit ?? 50);
  }

  /** A pull, branch switch or (without a checkout) clone, as its own job or as the first part of a build. */
  function updateSteps(p: Project, op: "pull" | "branch", branch: string): { kind: JobKind; steps: string[]; clone: boolean } {
    const clone = p.last_commit === "";
    const kind: JobKind = clone ? "project.clone" : op === "pull" ? "project.pull" : "project.branch";
    const steps = clone ? ["Cloning into repository", "Checking out files"] : op === "pull" ? ["Fetching origin", "Resetting to origin"] : [`Fetching ${branch}`, `Switched to branch '${branch}'`];
    return { kind, steps, clone };
  }

  function finishUpdate(p: Project, ok: boolean, clone: boolean, prev: Project["status"], branch: string) {
    // A failure before the tree is touched leaves the status as it was.
    p.status = ok ? "idle" : clone ? "error" : prev;
    if (!ok) return;
    p.branch = branch || "main";
    p.last_pulled_at = iso();
    p.last_commit = Math.random().toString(16).slice(2).padEnd(40, "0").slice(0, 40);
  }

  function startProjectUpdate(p: Project, op: "pull" | "branch", branch: string, me: Me): Job {
    const { kind, steps, clone } = updateSteps(p, op, branch);
    const prev = p.status;
    p.status = clone ? "cloning" : op === "pull" ? "pulling" : "switching_branch";
    if (failsGit(p.repo_url)) steps.push("FAIL");
    return startJob(st, me, false, kind, "project", p.id, steps, (ok) => finishUpdate(p, ok, clone, prev, branch), gitFailDetail(p.repo_url));
  }

  /** POST /projects/{id}/builds. Repositories with "fail" in their URL fail the pull; project 2 fails the preflight. */
  function createBuild(p: Project, req: CreateBuildRequest, me: Me): Response {
    const pull = req.pull === true;
    if (p.last_commit === "" && !pull) return fail(st, 409, "conflict", "the project has no checkout yet; pull it first (or build with pull)");
    if (activeJobFor(st, "project", p.id)) return fail(st, 409, "job_in_progress", "another job is already running for this project");
    if (p.status !== "idle" && !pull) return fail(st, 409, "conflict", "the project's checkout is in an error state; pull the project first (or build with pull)");
    if (st.builds.filter((b) => b.status === "queued" || b.status === "running").length >= MAX_ACTIVE_BUILDS) {
      return fail(st, 409, "conflict", `too many builds are queued (at most ${MAX_ACTIVE_BUILDS}); wait for one to finish`);
    }
    const b: Build = {
      id: st.nextBuildId++,
      project_id: p.id,
      branch: p.branch,
      commit: "",
      status: "queued",
      detail: "",
      job_id: null,
      image_id: "",
      image_tag: "",
      engine_version: "",
      schema_fingerprint: "",
      webtransport_path: "",
      websocket_path: "",
      server_ids: [],
      created_by: me.id,
      created_at: iso(),
      started_at: null,
      finished_at: null,
    };
    st.builds.push(b);
    const update = pull ? updateSteps(p, "pull", p.branch) : null;
    const prev = p.status;
    const steps = [
      ...(update ? update.steps : []),
      "# generated Dockerfile",
      "preflight: golem.yaml, go.mod, package.json",
      `#9 [build 4/9] RUN npm ci`,
      `#12 [build 6/9] RUN go run github.com/demiurgos-hub/golem-engine/cmd/golem-bake@${ENGINE}`,
      "#14 [build 8/9] RUN CGO_ENABLED=0 go build -trimpath ./cmd/server",
    ];
    const pullFails = pull && failsGit(p.repo_url);
    const preflightFails = p.id === 2;
    const pulled = update ? update.steps.length : 0;
    if (pullFails) steps.splice(pulled, steps.length, "FAIL");
    else if (preflightFails) steps.splice(pulled + 2, steps.length, "FAIL");
    const detail = pullFails
      ? `updating the checkout failed: ${gitFailDetail(p.repo_url)}`
      : "go.mod requires github.com/demiurgos-hub/golem-engine v0.3.2; Golem Deploy needs v0.4.0-rc.1 or newer";
    const job = startJob(st, me, false, "project.build", "project", p.id, steps, (ok) => {
      if (update) finishUpdate(p, !pullFails, update.clone, prev, p.branch);
      b.commit = p.last_commit;
      b.branch = p.branch;
      b.finished_at = iso();
      b.engine_version = preflightFails ? "v0.3.2" : ENGINE;
      if (job.status === "cancelled") {
        b.status = "cancelled";
        b.detail = "cancelled";
        return;
      }
      if (!ok) {
        b.status = "error";
        b.detail = detail;
        return;
      }
      b.status = "success";
      b.image_id = `sha256:${fakeFingerprint(`${p.id}-${b.id}-${b.commit}`)}`;
      b.image_tag = `golem-deploy/${p.name}:${b.commit.slice(0, 12)}-b${b.id}`;
      b.webtransport_path = "/wt";
      b.websocket_path = "/api/ws";
      st.images.unshift({
        id: b.image_id,
        tags: [b.image_tag],
        project_id: p.id,
        commit: b.commit,
        size_bytes: 190_000_000,
        created_at: iso(),
        server_ids: [],
        build_ids: [b.id],
        in_use: true,
      });
    }, detail);
    b.job_id = job.id;
    if (update) p.status = update.clone ? "cloning" : "pulling";
    // The job's own status drives the build's (running once the job runs; cancelled with it).
    const watch = setInterval(() => {
      if (b.status === "queued" && job.status === "running") {
        b.status = "running";
        b.started_at = iso();
      }
      if (job.status === "cancelled" && (b.status === "queued" || b.status === "running")) {
        b.status = "cancelled";
        b.detail = "cancelled";
        b.finished_at = iso();
        if (update) p.status = prev === "error" ? "error" : "idle";
      }
      if (b.status !== "queued" && b.status !== "running") clearInterval(watch);
    }, 100);
    return json(st, 202, { build: { ...b }, job });
  }

  /** GET /servers/{id}/runtime of a running server, as the game's admin endpoint would report it. */
  function runtime(s: Server): Response {
    if (s.name === "flood-test") return fail(st, 503, "service_unavailable", "the game's admin endpoint did not answer; see the server log");
    const b = st.builds.find((x) => x.id === s.build_id);
    const uptime = s.started_at ? (Date.now() - new Date(s.started_at).getTime()) / 1000 : 0;
    const loading = s.name === "kart-eu";
    const tick = Math.round(uptime * 30);
    const body: ServerRuntime = {
      ready: !loading,
      not_ready_reason: loading ? "loading world chunks (12/40)" : "",
      sessions: loading ? 0 : 3 + (Math.round(uptime / 7) % 9),
      entities: loading ? 0 : 412 + (Math.round(uptime) % 50),
      uptime_seconds: uptime,
      tick: { tick, rate: 30, budget_ms: 33.3, last_ms: 4 + (tick % 7) * 0.9, max_ms: 21.7, overruns: Math.floor(tick / 5000) },
      game_name: st.projects.find((p) => p.id === s.project_id)?.name ?? "game",
      engine_version: s.engine_version || ENGINE,
      transport: "webtransport",
      schema_fingerprint: b?.schema_fingerprint || FINGERPRINT,
      certificate_hashes_published: st.certMode === "self_signed",
      sampled_at: iso(),
    };
    return json(st, 200, body);
  }

  function startServerAction(s: Server, action: "start" | "stop" | "restart", me: Me): Job {
    s.desired_state = action === "stop" ? "stopped" : "running";
    s.updated_at = iso();
    const steps = action === "stop" ? [`stopping container golem-deploy-${s.name}`] : ["ensuring network golem-deploy", `starting container golem-deploy-${s.name}`, "waiting for the game to report ready", "ready"];
    let failDetail = "the container exited right after starting (exit code 1); its last output is in the job log";
    const build = st.builds.find((b) => b.id === s.build_id);
    if (action !== "stop" && !build) {
      steps.splice(0, steps.length, "FAIL");
      failDetail = "the server has no build; assign a successful build (PATCH /servers/{id} with build_id) and start it again";
    } else if (action !== "stop" && st.certMode === "custom" && !st.customCert) {
      steps.splice(0, steps.length, "FAIL");
      failDetail = "no valid custom game certificate is installed; upload one or switch to self_signed";
    } else if (s.name === "crashy" && action !== "stop") {
      steps.splice(2, steps.length, "container | panic: listen udp :7777: \\x1b[31mbind: address already in use\\x1b[0m", "container | exit status 1", "FAIL");
      failDetail = "the game exited with code 1 before it reported ready; see GET /servers/{id}/logs";
    }
    refreshDerived();
    if (action !== "stop" && build && s.data.schema_mismatch) {
      steps.splice(0, steps.length, "checking the saved data's schema", "FAIL");
      failDetail = `the saved data has schema ${s.data.schema_fingerprint.slice(0, 12)}, but build ${build.id} has schema ${build.schema_fingerprint.slice(0, 12)}; assign a build with the saved schema, restore a matching backup or reset the data`;
    }
    if (s.state === "unknown") {
      steps.splice(0, steps.length, `${action} server "${s.name}"`, "FAIL");
      failDetail = "Docker is unreachable";
    }
    return startJob(st, me, false, `server.${action}`, "server", s.id, steps, (ok) => {
      if (action === "stop") {
        if (ok && s.state !== "unknown") Object.assign(s, { state: "exited", exit_code: 0, started_at: null, restart_required: false });
        return;
      }
      // A start refused for the saved data's schema never touched the container.
      if (!build || s.state === "unknown" || (!ok && s.data.schema_mismatch)) return;
      // A start replaces the container: it now runs the configured build's image.
      containerImage.delete(s.id);
      if (st.certMode === "self_signed" && !st.serverCerts.has(s.id)) st.serverCerts.set(s.id, selfSignedCert(`server-${s.id}-${Date.now()}`));
      if (ok) {
        Object.assign(s, { state: "running", exit_code: null, started_at: iso(), restart_required: false, container_id: s.container_id || `c0ffee${s.id}0000` });
        containerStarted(s);
        if (!build.schema_fingerprint) build.schema_fingerprint = FINGERPRINT;
        // The game reports its schema when it becomes ready; a new world gets the build's.
        if (!s.data.volume) newVolume(s);
        if (!s.data.schema_fingerprint) s.data.schema_fingerprint = build.schema_fingerprint;
      } else {
        Object.assign(s, { state: "exited", exit_code: 1, started_at: null, container_id: s.container_id || `c0ffee${s.id}0000` });
        containerStarted(s);
      }
    }, failDetail);
  }

  /** POST /servers/{id}/backups: a fresh snapshot from a running game, or the files as they are. */
  function backupJob(s: Server, note: string, me: Me): Job {
    const running = s.state === "running";
    const steps = running
      ? ["asking the game for a fresh snapshot", "copying the data directory", "writing the archive"]
      : ["copying the data directory (the server is not running)", "writing the archive"];
    let detail = "";
    if (!s.data.volume) {
      steps.splice(0, steps.length, "FAIL");
      detail = "the server has no saved data yet; start it once so that the game creates its world";
    } else if (s.state === "unknown") {
      steps.splice(0, steps.length, "FAIL");
      detail = "Docker is unreachable";
    }
    const job: Job = startJob(st, me, false, "server.backup", "server", s.id, steps, (ok) => {
      if (ok) addBackup(s, "manual", job, me, { note, live: running });
    }, detail);
    return job;
  }

  /** The job detail when a safety backup leaves entries out: it was stored, and nothing was replaced. */
  function incompleteSafetyDetail(n: number, action: "restore" | "reset"): string {
    return `the safety backup would leave ${n} ${n === 1 ? "entry" : "entries"} of the data directory out (links, pipes, devices or names that are not allowed); it was stored, but nothing was replaced. Repeat the ${action} with skip_safety_backup to go ahead`;
  }

  /**
   * The job log lines of putting back a server whose restore, reset or deletion failed before its data was replaced: the
   * container that was running is started again as it was (never a new one from the assigned build).
   */
  function putBack(wasRunning: boolean, s: Server): string[] {
    return wasRunning ? [`starting container golem-deploy-${s.name} again with its previous data`, "the server is running again with its previous data"] : [];
  }

  /** The state after putBack: the same container runs again. */
  function putBackState(s: Server) {
    Object.assign(s, { state: "running", exit_code: null });
  }

  /** A server whose container was removed (after a reset, or a forced restore it cannot load): it has none until its next start. */
  function removeContainer(s: Server) {
    containerImage.delete(s.id);
    containerGone(s);
    Object.assign(s, { state: "missing", container_id: "", started_at: null, exit_code: null, restart_required: false });
  }

  /** POST /servers/{id}/restore: stop, safety backup, replace the data, start again if meant to run. */
  function restoreJob(s: Server, b: Backup, skipSafety: boolean, me: Me): Job {
    const wasActive = s.state === "running" || s.state === "restarting";
    const wasRunning = s.state === "running";
    const safety = !skipSafety && s.data.volume !== "";
    const omitted = safety ? (st.omittedEntries.get(s.id) ?? 0) : 0;
    const steps = [
      ...(wasActive ? [`stopping container golem-deploy-${s.name}`] : []),
      ...(safety ? ["taking a safety backup of the current data"] : []),
      ...(omitted > 0
        ? [...putBack(wasRunning, s), "FAIL"]
        : [
            `replacing the data with backup ${b.id}`,
            ...(s.desired_state === "running" ? [`starting container golem-deploy-${s.name}`, "waiting for the game to report ready"] : []),
          ]),
    ];
    const job: Job = startJob(st, me, true, "server.restore", "server", s.id, steps, (ok) => {
      if (safety) addBackup(s, "pre_restore", job, me);
      if (!ok) {
        if (wasRunning) putBackState(s);
        return;
      }
      // The backup was deleted while the restore was queued (a running restore protects it).
      if (!st.backups.some((x) => x.id === b.id)) {
        job.status = "error";
        job.detail = `backup ${b.id} no longer exists; nothing was replaced`;
        return;
      }
      if (s.data.volume) dropVolume(s.data.volume);
      newVolume(s);
      s.data.schema_fingerprint = b.schema_fingerprint;
      // The restored directory holds only what the backup stored.
      st.omittedEntries.delete(s.id);
      refreshDerived();
      // A forced restore of another schema leaves the server without a container until a build that can load it is assigned.
      if (s.data.schema_mismatch) removeContainer(s);
      else if (s.desired_state === "running") {
        Object.assign(s, { state: "running", started_at: iso(), exit_code: null, container_id: s.container_id || `c0ffee${s.id}0000`, restart_required: false });
        containerStarted(s);
      } else Object.assign(s, { state: s.container_id ? "exited" : "missing", started_at: null });
    }, incompleteSafetyDetail(omitted, "restore"));
    st.restoreReads.set(job.id, b.id);
    return job;
  }

  /** POST /servers/{id}/data/reset: stop, safety backup, discard the data, start with an empty world if meant to run. */
  function resetJob(s: Server, skipSafety: boolean, me: Me): Job {
    const wasActive = s.state === "running" || s.state === "restarting";
    const wasRunning = s.state === "running";
    const safety = !skipSafety && s.data.volume !== "";
    const omitted = safety ? (st.omittedEntries.get(s.id) ?? 0) : 0;
    const steps = [
      ...(wasActive ? [`stopping container golem-deploy-${s.name}`] : []),
      ...(safety ? ["taking a safety backup of the current data"] : []),
      ...(omitted > 0
        ? [...putBack(wasRunning, s), "FAIL"]
        : ["discarding the saved data", ...(s.desired_state === "running" ? [`starting container golem-deploy-${s.name}`, "waiting for the game to report ready"] : [])]),
    ];
    const job: Job = startJob(st, me, true, "server.data_reset", "server", s.id, steps, (ok) => {
      if (safety) addBackup(s, "pre_reset", job, me);
      if (!ok) {
        if (wasRunning) putBackState(s);
        return;
      }
      if (s.data.volume) dropVolume(s.data.volume);
      s.data.volume = "";
      s.data.schema_fingerprint = "";
      // The discarded data goes with whatever the directory held that could not be backed up.
      st.omittedEntries.delete(s.id);
      if (s.desired_state === "running") {
        newVolume(s);
        s.data.schema_fingerprint = st.builds.find((x) => x.id === s.build_id)?.schema_fingerprint ?? "";
        Object.assign(s, { state: "running", started_at: iso(), exit_code: null, container_id: s.container_id || `c0ffee${s.id}0000`, restart_required: false });
        containerStarted(s);
      } else {
        removeContainer(s);
      }
      refreshDerived();
    }, incompleteSafetyDetail(omitted, "reset"));
    return job;
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
