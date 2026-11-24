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
});
