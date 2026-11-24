import { describe, expect, it } from "vitest";
import type { RawResponse, Transport, UploadRequest } from "@/api/client";
import { createMockTransport } from "@/mock/transport";
import { jsonResponse } from "@/test/fake-api";

// The dev-only mock follows the same contract as the daemon for the rules the
// web client depends on.

const admin = { id: 1, username: "alice", role: "admin", approved: true, created_at: "", auth: "session", api_token: null };
const real: Transport = {
  fetch: async (url) => (String(url).endsWith("/account") ? jsonResponse(200, admin) : new Response("unexpected", { status: 599 })),
  upload: async () => {
    throw new Error("unexpected real upload");
  },
};
const auth = { Authorization: "Bearer t", "Content-Type": "application/json" };

async function call(t: Transport, method: string, path: string, body?: unknown) {
  const res = await t.fetch(`/api/v1${path}`, { method, headers: auth, body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await res.text();
  return { status: res.status, body: text ? JSON.parse(text) : null };
}

describe("mock transport contract", () => {
  it("rejects auth none for SSH URLs and keeps credentials only on the same origin", async () => {
    const t = createMockTransport(real);
    const created = await call(t, "POST", "/projects", { name: "k", repo_url: "git@host:a/b.git", auth: { type: "none" } });
    expect(created).toMatchObject({ status: 400, body: { error: { code: "validation_failed", field: "auth.type" } } });
    // Project 3 is an https project with stored credentials.
    const moved = await call(t, "PATCH", "/projects/3", { repo_url: "https://other.example.com/team/sandbox.git" });
    expect(moved).toMatchObject({ status: 400, body: { error: { field: "auth" } } });
    const renamed = await call(t, "PATCH", "/projects/3", { repo_url: "https://git.example.com:443/team/renamed.git" });
    expect(renamed.status).toBe(200);
  });

  it("names env value errors by key, reserves GOLEM_ and locks the port of a running server", async () => {
    const t = createMockTransport(real);
    const build_id = 3;
    expect(await call(t, "POST", "/servers", { name: "a", build_id, env: { GOLEM_X: "1" } })).toMatchObject({ body: { error: { field: "env" } } });
    expect(await call(t, "POST", "/servers", { name: "a", build_id, env: { MOTD: "a\nb" } })).toMatchObject({ body: { error: { field: "env.MOTD" } } });
    expect((await call(t, "POST", "/servers", { name: "a", build_id, env: { PATH: "/x" } })).status).toBe(201);
    expect(await call(t, "PATCH", "/servers/11", { env_set: { MOTD: "a\u0000" } })).toMatchObject({ body: { error: { field: "env_set.MOTD" } } });
    expect(await call(t, "PATCH", "/servers/11", { port: 7050 })).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });
  });

  it("reports static usage, refuses uploads over the quota and a third concurrent upload", async () => {
    const t = createMockTransport(real);
    const list = await call(t, "GET", "/static");
    expect(list.body).toMatchObject({ used_bytes: expect.any(Number), quota_bytes: expect.any(Number), file_count: 3, max_files: 2000 });
    const upload = (files: File[]): Promise<RawResponse> => {
      const body = new FormData();
      for (const f of files) body.append("files", f); // no filename: the same File object (keeps the faked size)
      return t.upload({ url: "/api/v1/static", body, headers: { Authorization: "Bearer t" } } satisfies UploadRequest);
    };
    const huge = new File([new Uint8Array(1)], "huge.bin");
    Object.defineProperty(huge, "size", { value: 300 * 1024 * 1024 });
    const ok = new File(["x"], "small.bin");
    const tooBig = await upload([ok, huge]);
    expect(tooBig.status).toBe(507);
    // All or nothing: the small file of the refused request was not stored either.
    expect((await call(t, "GET", "/static")).body.items.map((f: { name: string }) => f.name)).not.toContain("small.bin");
    const results = await Promise.all([upload([new File(["1"], "a.bin")]), upload([new File(["2"], "b.bin")]), upload([new File(["3"], "c.bin")])]);
    expect(results.map((r) => r.status).sort()).toEqual([201, 201, 429]);
  });

  it("builds: no checkout without pull, a build runs to success with an image, in-use builds cannot be deleted", async () => {
    const t = createMockTransport(real);
    // Project 3 has no checkout (failed clone).
    expect(await call(t, "POST", "/projects/3/builds", {})).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });
    expect((await call(t, "GET", "/builds?status=bogus")).status).toBe(400);
    const started = await call(t, "POST", "/projects/1/builds", { pull: true });
    expect(started).toMatchObject({ status: 202, body: { build: { status: "queued", project_id: 1 }, job: { kind: "project.build", subject_type: "project" } } });
    expect(await call(t, "POST", "/projects/1/builds", {})).toMatchObject({ status: 409, body: { error: { code: "job_in_progress" } } });
    const id = started.body.build.id as number;
    let b = (await call(t, "GET", `/builds/${id}`)).body;
    for (let i = 0; i < 60 && (b.status === "queued" || b.status === "running"); i++) {
      await new Promise((r) => setTimeout(r, 250));
      b = (await call(t, "GET", `/builds/${id}`)).body;
    }
    expect(b).toMatchObject({ status: "success", image_tag: expect.stringContaining(`-b${id}`), webtransport_path: "/wt" });
    const listed = await call(t, "GET", "/projects/1/builds");
    expect(listed.body.items[0].id).toBe(id);
    const images = (await call(t, "GET", "/images")).body.items as { id: string; build_ids: number[] }[];
    expect(images.find((i) => i.id === b.image_id)?.build_ids).toEqual([id]);
    // Build 3 is the build of several servers.
    expect(await call(t, "DELETE", "/builds/3")).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });
    expect(await call(t, "DELETE", `/images/${encodeURIComponent(b.image_id)}`)).toMatchObject({ status: 409 });
    expect((await call(t, "DELETE", `/builds/${id}`)).status).toBe(204);
  }, 20_000);

  it("servers: a build of the same project only, job_in_progress for port and build changes, runtime 200/409/503", async () => {
    const t = createMockTransport(real);
    expect(await call(t, "POST", "/servers", { name: "x", build_id: 2 })).toMatchObject({ status: 400, body: { error: { field: "build_id" } } });
    // Server 12 runs project 1; build 4 belongs to project 2.
    expect(await call(t, "PATCH", "/servers/12", { build_id: 4 })).toMatchObject({ status: 400, body: { error: { field: "build_id" } } });
    expect((await call(t, "PATCH", "/servers/12", { build_id: 3 })).body).toMatchObject({ build_id: 3, project_id: 1 });
    expect(await call(t, "PATCH", "/servers/12", { allowed_origins: ["https://ok.example.com", "ftp://x"] })).toMatchObject({
      body: { error: { field: "allowed_origins[1]" } },
    });
    expect((await call(t, "POST", "/servers/12/start")).status).toBe(202);
    expect(await call(t, "PATCH", "/servers/12", { port: 7050 })).toMatchObject({ status: 409, body: { error: { code: "job_in_progress" } } });
    expect((await call(t, "GET", "/servers/11/runtime")).body).toMatchObject({ ready: true, transport: "webtransport" });
    expect(await call(t, "GET", "/servers/14/runtime")).toMatchObject({ status: 503, body: { error: { code: "service_unavailable" } } });
    expect(await call(t, "GET", "/servers/16/runtime")).toMatchObject({ status: 409 });
    // Servers created before builds can start only once one is assigned.
    expect((await call(t, "GET", "/servers/18")).body).toMatchObject({ build_id: null, project_id: null });
  });

  it("certificates, static add-ons and import format 2", async () => {
    const t = createMockTransport(real);
    expect((await call(t, "GET", "/certificates/game")).body).toMatchObject({ mode: "self_signed", certificate: null });
    expect(await call(t, "PUT", "/certificates/game", { mode: "self_signed", certificate_pem: "x" })).toMatchObject({ body: { error: { field: "certificate_pem" } } });
    const pem = "-----BEGIN CERTIFICATE-----\nA\n-----END CERTIFICATE-----";
    expect(await call(t, "PUT", "/certificates/game", { mode: "custom", certificate_pem: pem, private_key_pem: "nope" })).toMatchObject({
      body: { error: { field: "private_key_pem" } },
    });
    const custom = await call(t, "PUT", "/certificates/game", { mode: "custom", certificate_pem: pem, private_key_pem: "-----BEGIN PRIVATE KEY-----\nK\n-----END PRIVATE KEY-----" });
    expect(custom.body).toMatchObject({ certificate: { mode: "custom", certificate: { self_signed: false } }, job: null });
    expect(JSON.stringify(custom.body)).not.toContain("PRIVATE KEY");
    expect((await call(t, "GET", "/servers/11")).body).toMatchObject({ restart_required: true, certificate: { self_signed: false } });

    expect(await call(t, "PATCH", "/projects/1", { static_addons: [{ name: "sound_bank.bin", target: "a" }] })).toMatchObject({
      body: { error: { field: "static_addons[0].name" } },
    });
    expect(await call(t, "PATCH", "/projects/1", { static_addons: [{ name: "textures.zip", target: "../x" }] })).toMatchObject({
      body: { error: { field: "static_addons[0].target" } },
    });
    expect((await call(t, "PATCH", "/projects/1", { static_addons: [{ name: "textures.zip", target: "./assets/" }], allow_custom_dockerfile: true })).body).toMatchObject({
      static_addons: [{ name: "textures.zip", target: "assets" }],
      allow_custom_dockerfile: true,
    });
    expect(await call(t, "DELETE", "/static/textures.zip")).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });

    const exported = (await call(t, "GET", "/servers/export")).body;
    expect(exported.format_version).toBe(2);
    expect(exported.servers.find((s: { name: string }) => s.name === "eu-arena")).toMatchObject({ project: "arena-shooter", commit: expect.any(String) });
    expect(await call(t, "POST", "/servers/import", { format_version: 1, servers: [{ name: "n", image: "x" }] })).toMatchObject({ status: 400, body: { error: { code: "bad_request" } } });
    expect(await call(t, "POST", "/servers/import", { format_version: 2, servers: [{ name: "n", project: "private-sandbox" }] })).toMatchObject({
      body: { error: { field: "servers[0].project" } },
    });
    const imported = await call(t, "POST", "/servers/import", { format_version: 2, servers: [{ name: "n", project: "kart-racer" }] });
    expect(imported.body.created[0]).toMatchObject({ build_id: 4, project_id: 2, desired_state: "stopped" });
  });
});

describe("mock transport: saved data and backups", () => {
  async function finished(t: Transport, id: number) {
    let j = (await call(t, "GET", `/jobs/${id}`)).body;
    for (let i = 0; i < 60 && (j.status === "queued" || j.status === "running"); i++) {
      await new Promise((r) => setTimeout(r, 250));
      j = (await call(t, "GET", `/jobs/${id}`)).body;
    }
    return j;
  }

  it("refuses to start a server whose world has another schema and restores a matching backup", async () => {
    const t = createMockTransport(real);
    // "staging" runs build 1, but its world was saved by build 3.
    expect((await call(t, "GET", "/servers/12")).body.data).toMatchObject({ schema_mismatch: true, schema_fingerprint: "3b6f0c2a91d4e8f7" });
    const start = await call(t, "POST", "/servers/12/start");
    expect(await finished(t, start.body.id)).toMatchObject({ status: "error", detail: expect.stringContaining("restore a matching backup") });
    expect(await call(t, "POST", "/servers/12/restore", { backup_id: 6 })).toMatchObject({ status: 409, body: { error: { code: "schema_mismatch" } } });
    expect(await call(t, "POST", "/servers/12/restore", { backup_id: 999 })).toMatchObject({ status: 400, body: { error: { field: "backup_id" } } });
    const restore = await call(t, "POST", "/servers/12/restore", { backup_id: 5 });
    expect(restore).toMatchObject({ status: 202, body: { kind: "server.restore", restricted: true } });
    expect(await call(t, "POST", "/servers/12/backups", {})).toMatchObject({ status: 409, body: { error: { code: "job_in_progress" } } });
    expect(await call(t, "DELETE", "/backups/5")).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });
    expect(await finished(t, restore.body.id)).toMatchObject({ status: "success" });
    expect((await call(t, "GET", "/servers/12")).body).toMatchObject({ state: "running", data: { schema_mismatch: false, schema_fingerprint: "a0c3d9e1f2b47c55" } });
    // The replaced world was kept as a safety backup.
    expect((await call(t, "GET", "/backups?server_id=12")).body.items[0]).toMatchObject({ kind: "pre_restore", server_id: 12, schema_fingerprint: "3b6f0c2a91d4e8f7" });
  }, 30_000);

  it("validates the settings, protects current volumes and keeps the backups of deleted servers", async () => {
    const t = createMockTransport(real);
    expect(await call(t, "PATCH", "/servers/11", { snapshot_interval_seconds: 5 })).toMatchObject({ body: { error: { field: "snapshot_interval_seconds" } } });
    expect(await call(t, "PATCH", "/servers/11", { backup_schedule: { interval_hours: 1, keep: 0 } })).toMatchObject({ body: { error: { field: "backup_schedule.keep" } } });
    expect(await call(t, "POST", "/servers", { name: "n", build_id: 3, backup_schedule: { interval_hours: 721, keep: 7 } })).toMatchObject({
      body: { error: { field: "backup_schedule.interval_hours" } },
    });
    expect((await call(t, "PATCH", "/servers/16", { snapshot_interval_seconds: 0, backup_schedule: { interval_hours: 12, keep: 3 } })).body).toMatchObject({
      snapshot_interval_seconds: 0,
      backup_schedule: { interval_hours: 12, keep: 3 },
    });
    expect(await call(t, "POST", "/servers/16/backups", { note: "a\nb" })).toMatchObject({ body: { error: { field: "note" } } });
    const volumes = (await call(t, "GET", "/volumes")).body.items as { name: string; current: boolean }[];
    expect(volumes.find((v) => v.name === "golem-deploy-data-11-1")?.current).toBe(true);
    expect(await call(t, "DELETE", "/volumes/golem-deploy-data-11-1")).toMatchObject({ status: 409 });
    expect((await call(t, "DELETE", "/volumes/golem-deploy-data-11-0")).status).toBe(204);
    expect((await call(t, "DELETE", "/servers/11")).status).toBe(204);
    const backups = (await call(t, "GET", "/backups")).body;
    expect(backups.items.filter((b: { server_name: string }) => b.server_name === "eu-arena").every((b: { server_id: number | null }) => b.server_id === null)).toBe(true);
    expect(backups.total_bytes).toBeGreaterThan(0);
    expect((await call(t, "GET", "/volumes")).body.items.some((v: { name: string }) => v.name === "golem-deploy-data-11-1")).toBe(false);
  }, 20_000);
});
