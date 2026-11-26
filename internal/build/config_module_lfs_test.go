package build

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// server.module names the directory of go.mod below the game root; it
// follows the same path rules as the other paths.
func TestParseConfigModule(t *testing.T) {
	for in, want := range map[string]string{
		"workers/game-server":    "workers/game-server",
		"./workers/game-server/": "workers/game-server",
		".":                      ".",
		"./":                     ".",
	} {
		cfg, err := ParseConfig([]byte("server:\n  module: \"" + in + "\"\n"))
		if err != nil || cfg.Server.Module != want {
			t.Errorf("module %q: %q, %v; want %q", in, cfg.Server.Module, err, want)
		}
	}
	if cfg, err := ParseConfig([]byte("server:\n  package: ./cmd/x\n")); err != nil || cfg.Server.Module != "." {
		t.Fatalf("default module: %+v %v", cfg, err)
	}
	for name, in := range map[string]string{
		"parent":        "../server",
		"traversal":     "a/../../b",
		"absolute":      "/srv",
		"backslash":     `a\\b`,
		"git":           "a/.git",
		"dash":          "-x",
		"space":         "a b",
		"empty segment": "a//b",
		"too deep":      strings.Repeat("a/", maxPathDepth) + "a",
		"too long":      strings.Repeat("a", maxPathBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte("server:\n  module: \"" + in + "\"\n"))
			wantUserErr(t, err, "server.module")
		})
	}
	_, err := ParseConfig([]byte("server:\n  module: [a]\n"))
	wantUserErr(t, err, "not valid")
}

// lfs.include lists Git LFS path patterns; they end up in one
// "--include=" argument, so commas, white space, options and traversal
// are refused.
func TestParseConfigLFSInclude(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
lfs:
  include:
    - content/**
    - workers/unity/Assets/AssetsByType/Databases/*.bytes
    - "*.bin"
    - "**"
    - foo*
    - a/b-c_d.e/f
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"content/**", "workers/unity/Assets/AssetsByType/Databases/*.bytes", "*.bin", "**", "foo*", "a/b-c_d.e/f"}
	if !reflect.DeepEqual(cfg.LFS.Include, want) {
		t.Fatalf("%q", cfg.LFS.Include)
	}
	if cfg, err := ParseConfig([]byte("root: .\n")); err != nil || cfg.LFS.Include != nil {
		t.Fatalf("default: %+v %v", cfg, err)
	}
	if cfg, err := ParseConfig([]byte("lfs: {}\n")); err != nil || cfg.LFS.Include != nil {
		t.Fatalf("empty lfs mapping: %+v %v", cfg, err)
	}
	many := make([]string, maxLFSPatterns)
	for i := range many {
		many[i] = "a"
	}
	if _, err := ParseConfig([]byte("lfs:\n  include: [" + strings.Join(many, ",") + "]\n")); err != nil {
		t.Fatalf("%d patterns: %v", maxLFSPatterns, err)
	}
	long := make([]string, 17)
	for i := range long {
		long[i] = strings.Repeat("x", maxPathBytes)
	}

	for name, tc := range map[string]struct{ yaml, want string }{
		"empty list":       {"lfs:\n  include: []\n", "at least one pattern"},
		"too many":         {"lfs:\n  include: [" + strings.Join(many, ",") + ",a]\n", "at most 32 patterns"},
		"too long total":   {"lfs:\n  include: [" + strings.Join(long, ",") + "]\n", "in total"},
		"too long pattern": {"lfs:\n  include: [" + strings.Repeat("x", maxPathBytes+1) + "]\n", "at most 255 bytes"},
		"too deep":         {"lfs:\n  include: [" + strings.Repeat("a/", maxPathDepth) + "a]\n", "path segments"},
		"empty pattern":    {`lfs: {include: [""]}`, "must not be empty"},
		"comma":            {`lfs: {include: ["a,b"]}`, `pattern "a,b" may only contain`},
		"space":            {`lfs: {include: ["a b"]}`, "may only contain"},
		"tab":              {`lfs: {include: ["a\tb"]}`, `pattern "a\x09b" may only contain`},
		"newline":          {`lfs: {include: ["a\n--exclude=x"]}`, `pattern "a\x0a--exclude=x"`},
		"escape":           {`lfs: {include: ["a\e[31m"]}`, `\x1b`},
		"bidi":             {`lfs: {include: ["a` + string(rune(0x202e)) + `b"]}`, `pattern "a` + string(rune(0x5c)) + `u202eb"`},
		"option":           {`lfs: {include: ["--exclude=x"]}`, "must not start with '-'"},
		"dash segment":     {`lfs: {include: ["a/-x"]}`, "must not start with '-'"},
		"leading slash":    {`lfs: {include: [/content]}`, "relative path"},
		"trailing slash":   {`lfs: {include: [content/]}`, "empty segments"},
		"double slash":     {`lfs: {include: [a//b]}`, "empty segments"},
		"dot":              {`lfs: {include: [./content]}`, "'.' or '..'"},
		"dotdot":           {`lfs: {include: [content/../x]}`, "'.' or '..'"},
		"backslash":        {`lfs: {include: ["a\\b"]}`, "may only contain"},
		"brackets":         {`lfs: {include: ["a[bc]"]}`, "may only contain"},
		"question mark":    {`lfs: {include: ["a?"]}`, "may only contain"},
		"negation":         {`lfs: {include: ["!a"]}`, "may only contain"},
		"unicode":          {`lfs: {include: ["café"]}`, "may only contain"},
		"null":             {"lfs:\n  include:\n", "at least one pattern"},
		"explicit null":    {"lfs: {include: null}\n", "at least one pattern"},
		"number":           {"lfs: {include: 3}\n", "lfs.include must be a list of strings"},
		"not a list":       {"lfs:\n  include: content/**\n", "lfs.include must be a list of strings"},
		"list of maps":     {"lfs:\n  include: [{a: b}]\n", "lfs.include must be a list of strings"},
		"null item":        {"lfs: {include: [content/**, null]}\n", "lfs.include must be a list of strings"},
		"unknown key":      {"lfs:\n  exclude: [a]\n", "not valid"},
		"unquoted star":    {"lfs:\n  include:\n    - *.bin\n", "YAML"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.yaml))
			wantUserErr(t, err, tc.want)
			var ue *UserError
			if errors.As(err, &ue) && len(ue.Msg) > 300 {
				t.Fatalf("message not clipped: %d bytes", len(ue.Msg))
			}
		})
	}
}
