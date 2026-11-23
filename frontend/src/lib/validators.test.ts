import { describe, expect, it } from "vitest";
import {
  utf8Length,
  validateEnvKey,
  validatePassword,
  validatePort,
  validateRepoUrl,
  validateResourceName,
  validateStaticName,
  validateTokenName,
  validateUsername,
} from "@/lib/validators";
import { checkServerImport, describeImportField } from "@/lib/server-import";

describe("validateUsername", () => {
  it.each(["abc", "alice", "a.b_c-d", "0day", "x".repeat(32), "  Alice  "])("accepts %j", (u) => {
    expect(validateUsername(u)).toBeNull();
  });
  it.each(["", "ab", "_alice", ".alice", "al ice", "x".repeat(33), "alice!", "élodie"])("rejects %j", (u) => {
    expect(validateUsername(u)).not.toBeNull();
  });
});

describe("validatePassword", () => {
  it("counts bytes, not characters", () => {
    expect(utf8Length("é")).toBe(2);
    // 11 characters but 12 bytes: valid.
    expect(validatePassword("é" + "a".repeat(10))).toBeNull();
    // 11 ASCII bytes: too short.
    expect(validatePassword("a".repeat(11))).toMatch(/at least 12 bytes/);
    // 24 snowmen = 24 characters but 72 bytes: valid; one more byte is too long.
    expect(validatePassword("☃".repeat(24))).toBeNull();
    expect(validatePassword("☃".repeat(24) + "a")).toMatch(/at most 72 bytes/);
    expect(validatePassword("a".repeat(72))).toBeNull();
    expect(validatePassword("")).not.toBeNull();
  });
});

describe("validateEnvKey", () => {
  it.each(["A", "_", "MAX_PLAYERS", "a1", "_9"])("accepts %j", (k) => expect(validateEnvKey(k)).toBeNull());
  it.each(["", "1A", "A-B", "A B", "Ä", "A=B"])("rejects %j", (k) => expect(validateEnvKey(k)).not.toBeNull());
});

describe("validateResourceName", () => {
  it.each(["a", "eu-arena", "x1", "a".repeat(63)])("accepts %j", (n) => expect(validateResourceName(n)).toBeNull());
  it.each(["", "-a", "Arena", "a_b", "a.b", "a".repeat(64)])("rejects %j", (n) => expect(validateResourceName(n)).not.toBeNull());
});

describe("validateStaticName", () => {
  it.each(["maps.zip", "a", "sound_bank.v2.bin", "9-x", "x".repeat(128), "console.txt"])("accepts %j", (n) => expect(validateStaticName(n)).toBeNull());
  it.each(["", ".env", "-x", "_x", "a b", "a/b", "x".repeat(129), "café.zip", "notes.", "CON", "nul.txt", "com1.log", "LPT9"])("rejects %j", (n) => expect(validateStaticName(n)).not.toBeNull());
});

describe("validateTokenName", () => {
  it("counts characters and rejects control characters", () => {
    expect(validateTokenName("ci")).toBeNull();
    expect(validateTokenName("☃".repeat(64))).toBeNull();
    expect(validateTokenName("☃".repeat(65))).not.toBeNull();
    expect(validateTokenName("   ")).not.toBeNull();
    expect(validateTokenName("a\tb")).not.toBeNull();
  });
});

describe("validateRepoUrl", () => {
  it.each([
    "https://github.com/o/r.git",
    "https://github.com/golang/example",
    "HTTPS://git.example.com:8443/team/game.git",
    "ssh://git@host/o/r.git",
    "ssh://host:2222/o/r.git",
    "ssh://git@[::1]:22/o/r",
    "git@github.com:o/r.git",
    "git@host:/abs",
  ])("accepts %j", (u) => expect(validateRepoUrl(u)).toBeNull());
  it.each([
    "",
    "http://github.com/o/r",
    "/srv/repo",
    "file:///srv/repo",
    "git://host/r",
    "ext::sh -c x",
    "github.com/o/r",
    "https://user:pw@github.com/o/r",
    "https://github.com/o/r?x=1",
    "https://github.com",
    "https://bad_host/o/r",
    "https://github.com:99999/o/r",
    "ssh://git:pw@host/o/r",
    "-oProxyCommand=x@h:r",
    "git@host:-r",
    "git@host:",
    "C:\\repo",
  ])("rejects %j", (u) => expect(validateRepoUrl(u)).not.toBeNull());
  it("uses the daemon's wording", () => {
    expect(validateRepoUrl("http://github.com/o/r")).toBe("Repository URL must use https:// or ssh (plain http is not allowed).");
    expect(validateRepoUrl("https://me@github.com/o/r")).toBe(
      "Https repository URLs must not contain a user name or password; use the auth fields.",
    );
  });
});

describe("validateEnvKey reserved names", () => {
  it.each(["PATH", "path", "Home", "DOCKER_HOST", "docker_anything", "BUILDKIT_X", "HTTP_PROXY", "SystemRoot", "x".repeat(129)])(
    "rejects %j",
    (k) => expect(validateEnvKey(k)).not.toBeNull(),
  );
  it.each(["PATHS", "MY_HOME", "x".repeat(128)])("accepts %j", (k) => expect(validateEnvKey(k)).toBeNull());
});

describe("validatePort", () => {
  it("allows blank when optional", () => {
    expect(validatePort("", { optional: true })).toBeNull();
    expect(validatePort("", { optional: false })).not.toBeNull();
    expect(validatePort("7777", { optional: false })).toBeNull();
    expect(validatePort("0", { optional: true })).not.toBeNull();
    expect(validatePort("1023", { optional: true })).not.toBeNull();
    expect(validatePort("32768", { optional: true })).not.toBeNull();
    expect(validatePort("65536", { optional: true })).not.toBeNull();
    expect(validatePort("80a", { optional: true })).not.toBeNull();
  });
});

describe("checkServerImport", () => {
  it("accepts a valid export", () => {
    const r = checkServerImport(
      JSON.stringify({ format_version: 1, servers: [{ name: "a", image: "img:1", port: 7777, env: { K: "v" }, desired_state: "running" }] }),
    );
    expect(r.errors).toEqual([]);
    expect(r.value?.servers).toHaveLength(1);
  });

  it("lists every problem", () => {
    const r = checkServerImport(
      JSON.stringify({
        format_version: 2,
        servers: [
          { name: "a", image: "", desired_state: "running", port: 7777 },
          { name: "a", image: "x", desired_state: "maybe", port: 7777, env: { "1BAD": "v", path: "x", Key: "1", KEY: "2" }, extra: true },
        ],
      }),
    );
    expect(r.value).toBeNull();
    expect(r.errors).toEqual(
      expect.arrayContaining([
        "format_version must be 1.",
        "Server 1 (a): image is required.",
        "Server 2 (a): the name is also used by server 1.",
        "Server 2 (a): port 7777 is also used by server 1.",
        'Server 2 (a): desired_state must be "running" or "stopped".',
        `Server 2 (a): environment variable "1BAD": Use up to 128 letters, digits and '_', not starting with a digit.`,
        'Server 2 (a): environment variable "path": path is reserved by the daemon and cannot be used.',
        'Server 2 (a): environment variable "KEY" is listed twice (names are unique ignoring case).',
        'Server 2 (a): unknown field "extra".',
      ]),
    );
  });

  it("rejects non-JSON", () => {
    expect(checkServerImport("not json").errors).toEqual(["The file is not valid JSON. Choose a file saved with Export."]);
  });

  it("accepts an export without desired_state or port", () => {
    expect(checkServerImport(JSON.stringify({ format_version: 1, servers: [{ name: "a", image: "sha256:abc" }] })).errors).toEqual([]);
  });

  it("names the entry a daemon error points at", () => {
    const doc = { format_version: 1, servers: [{ name: "a", image: "x", desired_state: "stopped" as const }, { name: "b", image: "y", desired_state: "stopped" as const }] };
    expect(describeImportField("servers[1].image", doc)).toBe("Server 2 (b), image");
    expect(describeImportField("format_version", doc)).toBe("format_version");
    expect(describeImportField(undefined, doc)).toBeNull();
  });
});
