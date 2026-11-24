import { describe, expect, it } from "vitest";
import {
  sameRepoOrigin,
  utf8Length,
  validateEnvKey,
  validateEnvTotals,
  validateEnvValue,
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
  // Only GOLEM_ (any case) is reserved now; container variables never reach the docker CLI's environment.
  it.each(["GOLEM_X", "golem_port", "Golem_", "x".repeat(129), "1X"])("rejects %j", (k) => expect(validateEnvKey(k)).not.toBeNull());
  it.each(["PATH", "HOME", "DOCKER_HOST", "LD_PRELOAD", "HTTP_PROXY", "MY_GOLEM_X", "GOLEMX", "x".repeat(128)])("accepts %j", (k) =>
    expect(validateEnvKey(k)).toBeNull(),
  );
  it("explains the reservation", () => expect(validateEnvKey("golem_x")).toBe("Names starting with GOLEM_ are reserved for golem-deploy."));
});

describe("validateEnvValue", () => {
  it("rejects line breaks and NUL like the daemon", () => {
    expect(validateEnvValue("MOTD", "a\nb")).toBe("The value contains a line break (values must be a single line).");
    expect(validateEnvValue("MOTD", "a\rb")).toBe("The value contains a line break (values must be a single line).");
    expect(validateEnvValue("MOTD", "a\u0000b")).toBe("The value contains a NUL byte.");
  });
  it("accepts every other character", () => {
    expect(validateEnvValue("V", " quotes \" ' # = $ \\ tab\t \u202e é ☃")).toBeNull();
    expect(validateEnvValue("V", "")).toBeNull();
  });
  it("counts bytes of name and value against 32 KiB", () => {
    expect(validateEnvValue("K", "x".repeat(32 * 1024 - 1))).toBeNull();
    expect(validateEnvValue("K", "x".repeat(32 * 1024))).toMatch(/exceeds 32768 bytes/);
    // é is 2 bytes in UTF-8.
    expect(validateEnvValue("K", "é".repeat(16 * 1024))).toMatch(/exceeds 32768 bytes/);
  });
  it("limits count and total size", () => {
    expect(validateEnvTotals(Object.fromEntries(Array.from({ length: 129 }, (_, i) => [`K${i}`, ""])))).toMatch(/At most 128 variables/);
    expect(validateEnvTotals({ A: "x".repeat(32000), B: "x".repeat(32000), C: "x".repeat(2000) })).toMatch(/exceed 65536 bytes in total/);
    expect(validateEnvTotals({ A: "x" })).toBeNull();
  });
});

describe("sameRepoOrigin", () => {
  it.each([
    ["https://github.com/a/b.git", "https://github.com/c/d"],
    ["https://GitHub.com/a/b", "https://github.com:443/a/b"],
    ["ssh://git@host/a/b", "git@host:c/d"],
    ["ssh://git@host:22/a", "ssh://other@host/b"],
  ])("%s and %s share an origin", (a, b) => expect(sameRepoOrigin(a, b)).toBe(true));
  it.each([
    ["https://github.com/a/b", "https://gitlab.com/a/b"],
    ["https://host/a", "https://host:8443/a"],
    ["https://host/a", "ssh://git@host/a"],
    ["ssh://git@host/a", "ssh://git@host:2222/a"],
    ["https://host/a", "not a url"],
  ])("%s and %s do not", (a, b) => expect(sameRepoOrigin(a, b)).toBe(false));
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
  const COMMIT = "4f1c2a9e0b7d3c5a8e6f1b2c3d4e5f6a7b8c9d0e";

  it("accepts a valid format 2 export", () => {
    const r = checkServerImport(
      JSON.stringify({
        format_version: 2,
        servers: [{ name: "a", project: "arena", commit: COMMIT, port: 7777, allowed_origins: ["https://play.example.com"], env: { K: "v" }, desired_state: "running" }],
      }),
    );
    expect(r.errors).toEqual([]);
    expect(r.value?.servers).toHaveLength(1);
    expect(r.value?.format_version).toBe(2);
  });

  it("explains that a format 1 export (image references) cannot be imported", () => {
    const r = checkServerImport(JSON.stringify({ format_version: 1, servers: [{ name: "a", image: "img:1" }] }));
    expect(r.value).toBeNull();
    expect(r.errors[0]).toMatch(/format version 1 export .* needs format 2/);
    expect(r.errors).toContain('Server 1 (a): unknown field "image".');
  });

  it("lists every problem", () => {
    const r = checkServerImport(
      JSON.stringify({
        format_version: 3,
        servers: [
          { name: "a", project: "", desired_state: "running", port: 7777 },
          {
            name: "a",
            project: "Arena!",
            commit: "abc",
            allowed_origins: "https://x",
            desired_state: "maybe",
            port: 7777,
            env: { "1BAD": "v", golem_x: "x", Key: "1", KEY: "2", MOTD: "a\nb" },
            extra: true,
          },
        ],
      }),
    );
    expect(r.value).toBeNull();
    expect(r.errors).toEqual(
      expect.arrayContaining([
        "format_version must be 2.",
        "Server 1 (a): project is required (the server runs a build of that project). A server exported without a build has an empty project: fill it in.",
        "Server 2 (a): project must be a project name.",
        "Server 2 (a): commit must be a full lower-case commit hash (or empty for the newest successful build).",
        "Server 2 (a): allowed_origins must be a list of origins.",
        "Server 2 (a): the name is also used by server 1.",
        "Server 2 (a): port 7777 is also used by server 1.",
        'Server 2 (a): desired_state must be "running" or "stopped".',
        `Server 2 (a): environment variable "1BAD": Use up to 128 letters, digits and '_', not starting with a digit.`,
        'Server 2 (a): environment variable "golem_x": Names starting with GOLEM_ are reserved for golem-deploy.',
        "Server 2 (a): environment variable MOTD: The value contains a line break (values must be a single line).",
        'Server 2 (a): environment variable "KEY" is listed twice (names are unique ignoring case).',
        'Server 2 (a): unknown field "extra".',
      ]),
    );
  });

  it("accepts at most 128 servers per document", () => {
    const servers = Array.from({ length: 129 }, (_, i) => ({ name: `s${i}`, project: "arena" }));
    expect(checkServerImport(JSON.stringify({ format_version: 2, servers })).errors).toEqual([
      "The file contains 129 servers; one import accepts at most 128. Split it into several files.",
    ]);
    expect(checkServerImport(JSON.stringify({ format_version: 2, servers: servers.slice(0, 128) })).errors).toEqual([]);
  });

  it("rejects non-JSON", () => {
    expect(checkServerImport("not json").errors).toEqual(["The file is not valid JSON. Choose a file saved with Export."]);
  });

  it("accepts an export without commit, desired_state, port or origins", () => {
    expect(checkServerImport(JSON.stringify({ format_version: 2, servers: [{ name: "a", project: "arena", commit: "" }] })).errors).toEqual([]);
  });

  it("names the entry a daemon error points at", () => {
    const doc = {
      format_version: 2,
      servers: [
        { name: "a", project: "arena", desired_state: "stopped" as const },
        { name: "b", project: "kart", desired_state: "stopped" as const },
      ],
    };
    expect(describeImportField("servers[1].commit", doc)).toBe("Server 2 (b), commit");
    expect(describeImportField("servers[0].env.MOTD", doc)).toBe("Server 1 (a), environment variable MOTD");
    expect(describeImportField("servers[1].allowed_origins[0]", doc)).toBe("Server 2 (b), allowed origin 1");
    expect(describeImportField("format_version", doc)).toBe("format_version");
    expect(describeImportField(undefined, doc)).toBeNull();
  });
});
