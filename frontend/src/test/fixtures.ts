import type { Build, CertificateInfo, Image, Project, Server, ServerRuntime } from "@/api/types.gen";

// Wire objects with every field of the milestone 3 contract, for tests to override.

export const COMMIT = "4f1c2a9e0b7d3c5a8e6f1b2c3d4e5f6a7b8c9d0e";
export const IMAGE_ID = "sha256:" + "ab".repeat(32);

export function projectFixture(overrides: Partial<Project> = {}): Project {
  return {
    id: 5,
    name: "arena",
    repo_url: "https://github.com/example/arena.git",
    branch: "main",
    auth_type: "none",
    auth_username: "",
    has_secret: false,
    last_commit: COMMIT,
    last_pulled_at: "2026-11-20T10:00:00Z",
    status: "idle",
    static_addons: [],
    allow_custom_dockerfile: false,
    created_at: "2026-11-01T10:00:00Z",
    updated_at: "2026-11-20T10:00:00Z",
    ...overrides,
  };
}

export function buildFixture(overrides: Partial<Build> = {}): Build {
  return {
    id: 12,
    project_id: 5,
    branch: "main",
    commit: COMMIT,
    status: "success",
    detail: "",
    job_id: 300,
    image_id: IMAGE_ID,
    image_tag: `golem-deploy/arena:${COMMIT.slice(0, 12)}-b12`,
    engine_version: "v0.4.0-rc.1",
    schema_fingerprint: "",
    webtransport_path: "/wt",
    websocket_path: "/api/ws",
    server_ids: [],
    created_by: 1,
    created_at: "2026-11-20T10:00:00Z",
    started_at: "2026-11-20T10:00:01Z",
    finished_at: "2026-11-20T10:04:00Z",
    ...overrides,
  };
}

export function imageFixture(overrides: Partial<Image> = {}): Image {
  return {
    id: IMAGE_ID,
    tags: [`golem-deploy/arena:${COMMIT.slice(0, 12)}-b12`],
    project_id: 5,
    commit: COMMIT,
    size_bytes: 4_000_000,
    created_at: "2026-11-20T10:00:00Z",
    server_ids: [],
    build_ids: [12],
    in_use: true,
    ...overrides,
  };
}

export function certFixture(overrides: Partial<CertificateInfo> = {}): CertificateInfo {
  return {
    subject: "CN=localhost",
    issuer: "CN=localhost",
    dns_names: ["localhost"],
    ip_addresses: ["127.0.0.1", "::1"],
    not_before: "2026-11-20T09:00:00Z",
    not_after: "2099-12-03T09:00:00Z",
    fingerprint_sha256: "9f".repeat(32),
    self_signed: true,
    chain_length: 1,
    covers_game_host: true,
    ...overrides,
  };
}

export function serverFixture(overrides: Partial<Server> = {}): Server {
  return {
    id: 3,
    name: "echo",
    build_id: 12,
    project_id: 5,
    image: IMAGE_ID,
    commit: COMMIT,
    engine_version: "v0.4.0-rc.1",
    port: 7810,
    public_url: "https://localhost:7810/",
    allowed_origins: [],
    env_keys: ["GREETING"],
    desired_state: "running",
    state: "running",
    container_id: "c0ffee",
    started_at: "2026-11-20T10:00:00Z",
    exit_code: null,
    restart_required: false,
    certificate: null,
    created_at: "2026-11-20T10:00:00Z",
    updated_at: "2026-11-20T11:00:00Z",
    ...overrides,
  };
}

export function runtimeFixture(overrides: Partial<ServerRuntime> = {}): ServerRuntime {
  return {
    ready: true,
    not_ready_reason: "",
    sessions: 7,
    entities: 431,
    uptime_seconds: 3725,
    tick: { tick: 111_750, rate: 30, budget_ms: 33.3, last_ms: 4.2, max_ms: 21.7, overruns: 0 },
    game_name: "arena",
    engine_version: "v0.4.0-rc.1",
    transport: "webtransport",
    schema_fingerprint: "3b6f0c2a91d4e8f7",
    certificate_hashes_published: true,
    sampled_at: "2026-11-20T11:00:00Z",
    ...overrides,
  };
}
