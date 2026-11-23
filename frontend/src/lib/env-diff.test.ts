import { describe, expect, it } from "vitest";
import { computeEnvDiff, emptyDraft, type EnvDraft } from "@/lib/env-diff";

describe("computeEnvDiff", () => {
  const keys = ["MAX_PLAYERS", "ADMIN_TOKEN", "REGION"];

  it("has no changes for an untouched draft", () => {
    const r = computeEnvDiff(keys, emptyDraft());
    expect(r).toMatchObject({ hasChanges: false, valid: true, diff: { env_set: {}, env_unset: [] } });
  });

  it("sends replacements and additions in env_set and removals in env_unset", () => {
    const draft: EnvDraft = {
      existing: { MAX_PLAYERS: { kind: "replace", value: "32" }, ADMIN_TOKEN: { kind: "remove" }, REGION: { kind: "keep" } },
      added: [{ id: "r1", key: "NEW_KEY", value: "v" }],
    };
    const r = computeEnvDiff(keys, draft);
    expect(r.valid).toBe(true);
    expect(r.hasChanges).toBe(true);
    expect(r.diff).toEqual({ env_set: { MAX_PLAYERS: "32", NEW_KEY: "v" }, env_unset: ["ADMIN_TOKEN"] });
  });

  it("allows setting an empty value", () => {
    const r = computeEnvDiff(keys, { existing: { REGION: { kind: "replace", value: "" } }, added: [] });
    expect(r.diff.env_set).toEqual({ REGION: "" });
  });

  it("ignores blank new rows", () => {
    const r = computeEnvDiff(keys, { existing: {}, added: [{ id: "r1", key: "", value: "" }] });
    expect(r).toMatchObject({ hasChanges: false, valid: true });
  });

  it("rejects invalid keys, duplicates and keys that already exist", () => {
    const r = computeEnvDiff(keys, {
      existing: {},
      added: [
        { id: "bad", key: "1X", value: "v" },
        { id: "d1", key: "DUP", value: "a" },
        { id: "d2", key: "DUP", value: "b" },
        { id: "ex", key: "REGION", value: "eu" },
        { id: "ok", key: "OK", value: "1" },
      ],
    });
    expect(r.valid).toBe(false);
    expect(Object.keys(r.rowErrors).sort()).toEqual(["bad", "d1", "d2", "ex"]);
    expect(r.diff.env_set).toEqual({ OK: "1" });
    // A key is never both set and unset.
    expect(r.diff.env_unset).toEqual([]);
  });

  it("never puts a key in both lists", () => {
    const r = computeEnvDiff(["A"], { existing: { A: { kind: "remove" } }, added: [{ id: "x", key: "A", value: "1" }] });
    expect(r.valid).toBe(false);
    expect(r.diff.env_unset).toEqual(["A"]);
    expect(r.diff.env_set).toEqual({});
  });

  it("treats names as unique ignoring case and rejects reserved names, like the daemon", () => {
    const draft: EnvDraft = {
      existing: {},
      added: [
        { id: "r1", key: "max_players", value: "1" },
        { id: "r2", key: "Token", value: "a" },
        { id: "r3", key: "TOKEN", value: "b" },
        { id: "r4", key: "docker_host", value: "x" },
      ],
    };
    const r = computeEnvDiff(keys, draft);
    expect(r.valid).toBe(false);
    expect(r.rowErrors.r1).toMatch(/MAX_PLAYERS already exists; names are unique ignoring case/);
    expect(r.rowErrors.r2).toMatch(/listed twice/);
    expect(r.rowErrors.r3).toMatch(/listed twice/);
    expect(r.rowErrors.r4).toMatch(/reserved/);
    expect(r.diff.env_set).toEqual({});
  });
});
