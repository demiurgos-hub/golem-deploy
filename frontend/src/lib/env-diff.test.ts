import { describe, expect, it } from "vitest";
import { ApiError } from "@/api/errors";
import { computeEnvDiff, emptyDraft, envErrorsFrom, type EnvDraft } from "@/lib/env-diff";

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
        { id: "r4", key: "golem_port", value: "x" },
        { id: "r5", key: "DOCKER_HOST", value: "x" },
      ],
    };
    const r = computeEnvDiff(keys, draft);
    expect(r.valid).toBe(false);
    expect(r.rowErrors.r1).toMatch(/MAX_PLAYERS already exists; names are unique ignoring case/);
    expect(r.rowErrors.r2).toMatch(/listed twice/);
    expect(r.rowErrors.r3).toMatch(/listed twice/);
    expect(r.rowErrors.r4).toMatch(/reserved/);
    // DOCKER_* and PATH are ordinary container variables now.
    expect(r.rowErrors.r5).toBeUndefined();
    expect(r.diff.env_set).toEqual({ DOCKER_HOST: "x" });
  });

  it("checks values of new and replaced variables without echoing them", () => {
    const r = computeEnvDiff(keys, {
      existing: { REGION: { kind: "replace", value: "eu\nwest" } },
      added: [
        { id: "n1", key: "MOTD", value: "line1\r\nline2" },
        { id: "n2", key: "BIG", value: "x".repeat(40_000) },
        { id: "n3", key: "OK", value: "fine" },
      ],
    });
    expect(r.valid).toBe(false);
    expect(r.existingErrors.REGION).toBe("The value contains a line break (values must be a single line).");
    expect(r.rowErrors.n1).toBe("The value contains a line break (values must be a single line).");
    expect(r.rowErrors.n2).toMatch(/exceeds 32768 bytes/);
    expect(JSON.stringify(r)).not.toContain("line1");
    expect(r.diff.env_set).toEqual({ OK: "fine" });
  });

  it("checks the totals of a complete set (new server)", () => {
    const added = Array.from({ length: 129 }, (_, i) => ({ id: `r${i}`, key: `K${i}`, value: "" }));
    expect(computeEnvDiff([], { existing: {}, added }).general).toMatch(/At most 128 variables/);
  });

  it("routes the daemon's env errors to the row they name", () => {
    const draft: EnvDraft = { existing: { REGION: { kind: "replace", value: "x" } }, added: [{ id: "n1", key: "MOTD", value: "x" }] };
    const err = (field: string) => new ApiError({ status: 400, code: "validation_failed", message: "the value contains a NUL byte", field });
    expect(envErrorsFrom(err("env.MOTD"), draft, keys)?.rowErrors).toEqual({ n1: "The value contains a NUL byte." });
    expect(envErrorsFrom(err("env_set.REGION"), draft, keys)?.existingErrors).toEqual({ REGION: "The value contains a NUL byte." });
    expect(envErrorsFrom(err("env_set"), draft, keys)?.general).toBe("The value contains a NUL byte.");
    expect(envErrorsFrom(err("env_set.OTHER"), draft, keys)?.general).toBe("OTHER: The value contains a NUL byte.");
    expect(envErrorsFrom(err("image"), draft, keys)).toBeNull();
  });
});
