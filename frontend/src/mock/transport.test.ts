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
/** A daemon that knows the caller as another account with `role`. */
const realAs = (role: string): Transport => ({
  ...real,
  fetch: async (url) =>
    String(url).endsWith("/account") ? jsonResponse(200, { ...admin, id: 2, username: "dev", role }) : new Response("unexpected", { status: 599 }),
});
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
    // A running restore protects the backup it reads.
    expect(await running(t, restore.body.id)).toMatchObject({ status: "running" });
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

  async function running(t: Transport, id: number) {
    let j = (await call(t, "GET", `/jobs/${id}`)).body;
    for (let i = 0; i < 40 && j.status === "queued"; i++) {
      await new Promise((r) => setTimeout(r, 50));
      j = (await call(t, "GET", `/jobs/${id}`)).body;
    }
    return j;
  }

  it("protects a backup only while a restore that reads it runs", async () => {
    const t = createMockTransport(real);
    // Queued: the backup can still be deleted, and the restore then fails without replacing anything.
    const volume = (await call(t, "GET", "/servers/12")).body.data.volume;
    const queued = await call(t, "POST", "/servers/12/restore", { backup_id: 5 });
    expect((await call(t, "DELETE", "/backups/5")).status).toBe(204);
    expect(await finished(t, queued.body.id)).toMatchObject({ status: "error", detail: expect.stringContaining("no longer exists") });
    expect((await call(t, "GET", "/servers/12")).body.data.volume).toBe(volume);
  }, 30_000);

  it("leaves a server without a container after a reset or a forced restore it cannot load", async () => {
    const t = createMockTransport(real);
    // "staging" is stopped: after a reset it has no container until its next start.
    const reset = await call(t, "POST", "/servers/12/data/reset", {});
    expect(await finished(t, reset.body.id)).toMatchObject({ status: "success" });
    expect((await call(t, "GET", "/servers/12")).body).toMatchObject({ state: "missing", container_id: "", data: { volume: "" } });
    expect((await call(t, "GET", "/backups?server_id=12")).body.items[0]).toMatchObject({ kind: "pre_reset" });
    // "eu-arena" is meant to run, but backup 4 has the old schema: it stays without a container.
    const forced = await call(t, "POST", "/servers/11/restore", { backup_id: 4, force: true });
    expect(await finished(t, forced.body.id)).toMatchObject({ status: "success" });
    expect((await call(t, "GET", "/servers/11")).body).toMatchObject({ state: "missing", container_id: "", data: { schema_mismatch: true } });
  }, 30_000);

  it("stores a last backup before deleting a server and lets only admins skip it", async () => {
    const dev = createMockTransport(realAs("developer"));
    expect(await call(dev, "DELETE", "/servers/16?skip_safety_backup=1")).toMatchObject({ status: 403, body: { error: { code: "forbidden" } } });
    expect((await call(dev, "GET", "/servers/16")).status).toBe(200);
    const t = createMockTransport(real);
    expect((await call(t, "DELETE", "/servers/15")).status).toBe(204);
    expect((await call(t, "GET", "/backups")).body.items[0]).toMatchObject({ kind: "pre_delete", server_id: null, server_name: "kart-eu", omitted_entries: 0 });
    // A server without saved data has nothing to back up.
    const before = (await call(t, "GET", "/backups")).body.items.length;
    expect((await call(t, "DELETE", "/servers/16")).status).toBe(204);
    expect((await call(t, "GET", "/backups")).body.items).toHaveLength(before);
  }, 30_000);

  it("stops a deletion, reset or restore whose safety backup leaves entries out, unless it is skipped", async () => {
    const t = createMockTransport(real);
    // "flood-test" keeps entries in its data directory that a backup cannot hold.
    const seeded = (await call(t, "GET", "/backups?server_id=14")).body.items[0];
    expect(seeded).toMatchObject({ omitted_entries: 2 });
    const del = await call(t, "DELETE", "/servers/14");
    expect(del).toMatchObject({ status: 409, body: { error: { code: "conflict", message: expect.stringContaining("skip_safety_backup=1") } } });
    expect((await call(t, "GET", "/servers/14")).status).toBe(200);
    expect((await call(t, "GET", "/backups?server_id=14")).body.items[0]).toMatchObject({ kind: "pre_delete", omitted_entries: 2 });

    const volume = (await call(t, "GET", "/servers/14")).body.data.volume;
    const reset = await call(t, "POST", "/servers/14/data/reset", {});
    expect(await finished(t, reset.body.id)).toMatchObject({ status: "error", detail: expect.stringContaining("nothing was replaced") });
    expect((await call(t, "GET", "/servers/14")).body.data.volume).toBe(volume);
    expect((await call(t, "GET", "/backups?server_id=14")).body.items[0]).toMatchObject({ kind: "pre_reset", omitted_entries: 2 });
    const restore = await call(t, "POST", "/servers/14/restore", { backup_id: seeded.id });
    expect(await finished(t, restore.body.id)).toMatchObject({ status: "error", detail: expect.stringContaining("skip_safety_backup") });

    const skipped = await call(t, "POST", "/servers/14/data/reset", { skip_safety_backup: true });
    expect(await finished(t, skipped.body.id)).toMatchObject({ status: "success" });
    expect((await call(t, "GET", "/servers/14")).body.data.volume).not.toBe(volume);
    expect((await call(t, "DELETE", "/servers/14?skip_safety_backup=1")).status).toBe(204);
  }, 40_000);

  async function jobLog(t: Transport, id: number) {
    return (await t.fetch(`/api/v1/jobs/${id}/log`, { method: "GET", headers: auth })).text();
  }

  it("deletes a server only when its delete job succeeds and parses skip_safety_backup like the daemon", async () => {
    const t = createMockTransport(real);
    for (const q of ["", "yes", "TRUE", "1&skip_safety_backup=1", "0&skip_safety_backup=1"]) {
      expect(await call(t, "DELETE", `/servers/15?skip_safety_backup=${q}`), q).toMatchObject({ status: 400, body: { error: { code: "bad_request" } } });
    }
    expect((await call(t, "GET", "/servers/15")).status).toBe(200);
    // A cancelled delete job deletes nothing.
    const pending = call(t, "DELETE", "/servers/15?skip_safety_backup=false");
    let job: { id: number } | undefined;
    for (let i = 0; i < 40 && !job; i++) {
      await new Promise((r) => setTimeout(r, 25));
      job = (await call(t, "GET", "/jobs?subject_type=server&subject_id=15")).body.items.find((j: { kind: string }) => j.kind === "server.delete");
    }
    expect((await call(t, "POST", `/jobs/${job?.id}/cancel`)).status).toBe(202);
    expect(await pending).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });
    expect((await call(t, "GET", "/servers/15")).status).toBe(200);
    expect((await call(t, "GET", "/backups?server_id=15")).body.items.some((b: { kind: string }) => b.kind === "pre_delete")).toBe(false);
    expect((await call(t, "DELETE", "/servers/15?skip_safety_backup=0")).status).toBe(204);
  }, 30_000);

  it("starts the same container again when a safety backup fails", async () => {
    const t = createMockTransport(real);
    // "flood-test" runs, and its safety backups leave entries out.
    const before = (await call(t, "GET", "/servers/14")).body;
    expect(before).toMatchObject({ state: "running" });
    const reset = await call(t, "POST", "/servers/14/data/reset", {});
    expect(await finished(t, reset.body.id)).toMatchObject({ status: "error" });
    expect(await jobLog(t, reset.body.id)).toContain("the server is running again with its previous data");
    expect((await call(t, "GET", "/servers/14")).body).toMatchObject({ state: "running", container_id: before.container_id });
    expect(await call(t, "DELETE", "/servers/14")).toMatchObject({ status: 409 });
    const del = (await call(t, "GET", "/jobs?subject_type=server&subject_id=14")).body.items.find((j: { kind: string }) => j.kind === "server.delete");
    expect(await jobLog(t, del.id)).toContain("the server is running again with its previous data");
    expect((await call(t, "GET", "/servers/14")).body).toMatchObject({ state: "running", container_id: before.container_id });
  }, 30_000);

  it("checks backup notes like the daemon and stores them trimmed", async () => {
    const t = createMockTransport(real);
    for (const bad of [0x09, 0x7f, 0x85, 0x2028, 0x202e, 0x2067]) {
      expect(await call(t, "POST", "/servers/11/backups", { note: `a${String.fromCodePoint(bad)}b` }), `U+${bad.toString(16)}`).toMatchObject({
        status: 400,
        body: { error: { field: "note" } },
      });
    }
    expect(await call(t, "POST", "/servers/11/backups", { note: "x".repeat(201) })).toMatchObject({ status: 400, body: { error: { field: "note" } } });
    const ok = await call(t, "POST", "/servers/11/backups", { note: "  before the patch \n" });
    expect(await finished(t, ok.body.id)).toMatchObject({ status: "success" });
    expect((await call(t, "GET", "/backups?server_id=11")).body.items[0]).toMatchObject({ kind: "manual", note: "before the patch" });
  }, 30_000);
});

describe("mock transport: the Inspector", () => {
  async function finished(t: Transport, id: number) {
    let j = (await call(t, "GET", `/jobs/${id}`)).body;
    for (let i = 0; i < 80 && (j.status === "queued" || j.status === "running"); i++) {
      await new Promise((r) => setTimeout(r, 250));
      j = (await call(t, "GET", `/jobs/${id}`)).body;
    }
    return j;
  }
  /** A daemon that knows the caller through an API token. */
  const realToken: Transport = {
    ...real,
    fetch: async (url) =>
      String(url).endsWith("/account")
        ? jsonResponse(200, { ...admin, auth: "api_token", api_token: { id: 4, name: "ci" } })
        : new Response("unexpected", { status: 599 }),
  };

  it("lets every identity read it, admins change it and only login sessions open it", async () => {
    const t = createMockTransport(real);
    const view = (await call(t, "GET", "/inspector")).body;
    expect(view).toMatchObject({ available: true, enabled: true, state: "running", port: 7700, public_url: "", agents_reachable: true, other_targets: 0 });
    // eu-arena is connected; flood-test was started with the Inspector, but its game never connects.
    expect(view.targets).toEqual([expect.objectContaining({ server_id: 11, connected: true })]);
    expect((await call(t, "GET", "/servers/14")).body).toMatchObject({ inspector: true, inspector_active: true });

    const dev = createMockTransport(realAs("developer"));
    expect((await call(dev, "GET", "/inspector")).status).toBe(200);
    expect(await call(dev, "PATCH", "/inspector", { enabled: false })).toMatchObject({ status: 403, body: { error: { code: "forbidden" } } });
    expect((await call(dev, "POST", "/inspector/sessions", {})).status).toBe(200);

    const token = createMockTransport(realToken);
    expect((await call(token, "GET", "/inspector")).status).toBe(200);
    expect(await call(token, "POST", "/inspector/sessions", {})).toMatchObject({ status: 403, body: { error: { code: "session_required" } } });

    const session = await call(t, "POST", "/inspector/sessions", { server_id: 11 });
    expect(session).toMatchObject({ status: 200, body: { port: 7700, public_url: "", path: expect.stringMatching(/^\/_golem-deploy\/enter#t=[A-Za-z0-9_-]{43}$/) } });
    expect(Date.parse(session.body.expires_at) - Date.now()).toBeLessThanOrEqual(60_000);
    // Every link is a new one.
    expect((await call(t, "POST", "/inspector/sessions", { server_id: 11 })).body.path).not.toBe(session.body.path);
    expect(await call(t, "POST", "/inspector/sessions", { server_id: 999 })).toMatchObject({ status: 404, body: { error: { code: "not_found" } } });
    // "staging" has the Inspector switch off.
    expect(await call(t, "POST", "/inspector/sessions", { server_id: 12 })).toMatchObject({ status: 400, body: { error: { field: "server_id" } } });
  });

  it("turns the Inspector off and on with a restricted job and marks the servers that need a restart", async () => {
    const t = createMockTransport(real);
    expect(await call(t, "PATCH", "/inspector", { enabled: false, pull_image: true })).toMatchObject({ status: 400, body: { error: { field: "pull_image" } } });
    const off = await call(t, "PATCH", "/inspector", { enabled: false });
    expect(off).toMatchObject({ status: 202, body: { kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", restricted: true } });
    expect(await call(t, "PATCH", "/inspector", {})).toMatchObject({ status: 409, body: { error: { code: "job_in_progress" } } });
    expect((await call(t, "GET", "/jobs?subject_type=inspector&subject_id=hub")).body.items[0]).toMatchObject({ id: off.body.id });
    expect(await finished(t, off.body.id)).toMatchObject({ status: "success" });
    expect((await call(t, "GET", "/inspector")).body).toMatchObject({ enabled: false, state: "off", targets: null, started_at: null, image_id: "" });
    expect(await call(t, "POST", "/inspector/sessions", {})).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });
    expect(await call(t, "PATCH", "/inspector", { rotate_secrets: true })).toMatchObject({ status: 400, body: { error: { field: "rotate_secrets" } } });
    // eu-arena's container was started with the Inspector.
    expect((await call(t, "GET", "/servers/11")).body).toMatchObject({ inspector_active: true, restart_required: true });

    // A server created with the switch on while the Inspector is off starts without it.
    const created = await call(t, "POST", "/servers", { name: "probe", build_id: 3, inspector: true, desired_state: "running" });
    expect(created).toMatchObject({ status: 201, body: { inspector: true, inspector_active: false } });
    const start = (await call(t, "GET", `/jobs?subject_type=server&subject_id=${created.body.id}`)).body.items[0];
    expect(await finished(t, start.id)).toMatchObject({ status: "success" });
    expect((await call(t, "GET", `/servers/${created.body.id}`)).body).toMatchObject({ state: "running", inspector_active: false, restart_required: false });

    const on = await call(t, "PATCH", "/inspector", { enabled: true });
    expect((await call(t, "GET", "/inspector")).body).toMatchObject({ enabled: true, state: "stopped" });
    expect(await finished(t, on.body.id)).toMatchObject({ status: "success" });
    expect((await call(t, "GET", "/inspector")).body).toMatchObject({ enabled: true, state: "running", targets: [expect.objectContaining({ server_id: 11 })] });
    expect((await call(t, "GET", `/servers/${created.body.id}`)).body).toMatchObject({ inspector_active: false, restart_required: true });
    const restart = await call(t, "POST", `/servers/${created.body.id}/restart`);
    expect(await finished(t, restart.body.id)).toMatchObject({ status: "success" });
    expect((await call(t, "GET", `/servers/${created.body.id}`)).body).toMatchObject({ inspector_active: true, restart_required: false });
    expect((await call(t, "GET", "/inspector")).body.targets.map((x: { server_id: number }) => x.server_id)).toContain(created.body.id);
  }, 60_000);

  it("disconnects every server when the secrets are replaced, and keeps the switch in exports and imports", async () => {
    const t = createMockTransport(real);
    const rotate = await call(t, "PATCH", "/inspector", { rotate_secrets: true });
    expect(rotate.status).toBe(202);
    expect(await finished(t, rotate.body.id)).toMatchObject({ status: "success" });
    expect((await call(t, "GET", "/inspector")).body).toMatchObject({ state: "running", targets: [] });
    expect((await call(t, "GET", "/servers/11")).body).toMatchObject({ inspector_active: true, restart_required: true });

    // The switch applies on the next (re)start: a running server needs a restart, a stopped one does not.
    expect(await call(t, "PATCH", "/servers/16", { inspector: "yes" })).toMatchObject({ status: 400, body: { error: { code: "bad_request" } } });
    expect((await call(t, "PATCH", "/servers/16", { inspector: true })).body).toMatchObject({ inspector: true, restart_required: false });
    expect((await call(t, "PATCH", "/servers/12", { inspector: true })).body).toMatchObject({ inspector: true, restart_required: false });
    expect((await call(t, "PATCH", "/servers/14", { inspector: false })).body).toMatchObject({ inspector: false, inspector_active: true, restart_required: true });

    const exported = (await call(t, "GET", "/servers/export")).body;
    const byName = new Map<string, Record<string, unknown>>(exported.servers.map((s: { name: string }) => [s.name, s]));
    expect(byName.get("eu-arena")).toMatchObject({ inspector: true });
    expect(byName.get("crashy")).not.toHaveProperty("inspector");
    const spec = { name: "copy", project: "arena-shooter" };
    expect(await call(t, "POST", "/servers/import", { format_version: 2, servers: [{ ...spec, inspector: 1 }] })).toMatchObject({ status: 400 });
    const imported = await call(t, "POST", "/servers/import", { format_version: 2, servers: [{ ...spec, inspector: true }] });
    expect(imported).toMatchObject({ status: 201, body: { created: [{ name: "copy", inspector: true, inspector_active: false }] } });
  }, 30_000);
});
