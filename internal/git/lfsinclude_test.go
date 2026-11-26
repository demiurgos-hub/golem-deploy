package git

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// The include patterns become exactly one stuck "--include=" argument, in
// order; no pattern can become an argv element of its own.
func TestLFSPullArgsInclude(t *testing.T) {
	args := lfsPullArgs([]string{"b/**", "a/*.bin"})
	if !slices.Equal(args, []string{"lfs", "pull", "--include=b/**,a/*.bin"}) {
		t.Fatalf("%q", args)
	}
	for _, a := range args[2:] {
		if !strings.HasPrefix(a, "--include=") {
			t.Fatalf("separate argument %q", a)
		}
	}
}

func TestCheckLFSPatterns(t *testing.T) {
	for _, ok := range [][]string{nil, {"content/**"}, {"*.bytes", "**", "foo*", "a/b-c_d.e/f*"}} {
		if err := checkLFSPatterns(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	many := make([]string, maxLFSPatterns+1)
	for i := range many {
		many[i] = "a"
	}
	for _, bad := range [][]string{
		{""}, {"-x"}, {"--exclude=a"}, {"a/-x"}, {"/abs"}, {"a,b"}, {"a b"}, {"a\tb"}, {"a\nb"}, {"a\x00"},
		{"ok", "a\\b"}, {"a?"}, {"[ab]"}, {"!a"}, many, {strings.Repeat("a", maxLFSIncludeBytes+1)},
	} {
		if err := checkLFSPatterns(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// lfsIncludes logs one line when the download is limited, fails with
// ReasonDeployConfig and the injected (sanitised) message when the
// configuration is invalid, and is a no-op without the option or the key.
func TestLFSIncludesSession(t *testing.T) {
	newSession := func(fn func(string) ([]string, error)) (*session, *bytes.Buffer) {
		var buf bytes.Buffer
		return &session{r: &Runner{lfsInclude: fn}, log: newScrubber(&buf, nil)}, &buf
	}
	s, _ := newSession(nil)
	if p, err := s.lfsIncludes("pull", "/x"); p != nil || err != nil {
		t.Fatalf("no option: %q %v", p, err)
	}
	var gotDir string
	s, _ = newSession(func(dir string) ([]string, error) { gotDir = dir; return nil, nil })
	if p, err := s.lfsIncludes("pull", "/checkout"); p != nil || err != nil || gotDir != "/checkout" {
		t.Fatalf("no key: %q %v %q", p, err, gotDir)
	}

	s, buf := newSession(func(string) ([]string, error) { return []string{"content/**", "*.bytes"}, nil })
	p, err := s.lfsIncludes("pull", "/x")
	s.log.Flush()
	if err != nil || !slices.Equal(p, []string{"content/**", "*.bytes"}) {
		t.Fatalf("%q %v", p, err)
	}
	if !strings.Contains(buf.String(), "Git LFS: downloading only the paths listed under lfs.include in golem-deploy.yaml (2 patterns)") ||
		strings.Contains(buf.String(), "content/**") {
		t.Fatalf("log %q", buf.String())
	}
	s, buf = newSession(func(string) ([]string, error) { return []string{"a"}, nil })
	_, _ = s.lfsIncludes("pull", "/x")
	s.log.Flush()
	if !strings.Contains(buf.String(), "(1 pattern)") {
		t.Fatalf("log %q", buf.String())
	}

	msg := `golem-deploy.yaml: lfs.include pattern "a,b" may only contain letters` + "\x1b[31m\n" + strings.Repeat("x", 500)
	s, buf = newSession(func(string) ([]string, error) { return nil, errors.New(msg) })
	_, err = s.lfsIncludes("clone", "/x")
	s.log.Flush()
	var ge *Error
	if !errors.As(err, &ge) || ge.Reason != ReasonDeployConfig {
		t.Fatalf("%v", err)
	}
	m := ge.Message()
	if !strings.HasPrefix(m, `golem-deploy.yaml: lfs.include pattern "a,b"`) || len(m) > maxDetail+3 ||
		strings.ContainsAny(m, "\x1b\n") || SafeMessage(err) != m {
		t.Fatalf("message %q", m)
	}
	if !strings.Contains(buf.String(), `error: golem-deploy.yaml: lfs.include pattern "a,b"`) {
		t.Fatalf("log %q", buf.String())
	}

	// Patterns that slipped past the parser are refused here too.
	s, _ = newSession(func(string) ([]string, error) { return []string{"--exclude=x"}, nil })
	if _, err := s.lfsIncludes("pull", "/x"); !errors.As(err, &ge) || ge.Reason != ReasonDeployConfig ||
		!strings.Contains(ge.Message(), "golem-deploy.yaml") {
		t.Fatalf("%v", err)
	}
	if m := (&Error{Reason: ReasonDeployConfig}).Message(); !strings.Contains(m, "golem-deploy.yaml") {
		t.Fatalf("fallback message %q", m)
	}
}
