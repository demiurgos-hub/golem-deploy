package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestG1_LFSEndpointDerivation pins git-lfs's own derivation rules: https
// gets "<url without trailing slash>[.git]/info/lfs", SSH stays the remote.
func TestG1_LFSEndpointDerivation(t *testing.T) {
	for in, want := range map[string]string{
		"https://h.example/team/game.git":        "https://h.example/team/game.git/info/lfs",
		"https://h.example/team/game":            "https://h.example/team/game.git/info/lfs",
		"https://h.example/team/game/":           "https://h.example/team/game.git/info/lfs",
		"https://h.example/team/game.git/":       "https://h.example/team/game.git/info/lfs",
		"https://h.example:8443/game.git":        "https://h.example:8443/game.git/info/lfs",
		"https://[::1]:8443/game":                "https://[::1]:8443/game.git/info/lfs",
		"ssh://git@h.example:2222/team/game.git": "ssh://git@h.example:2222/team/game.git",
		"git@h.example:team/game.git":            "git@h.example:team/game.git",
		"http://h.example/x":                     "",
	} {
		if got := LFSEndpoint(in); got != want {
			t.Errorf("LFSEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestG1_DisallowedLFSKeys: endpoint-affecting keys in .lfsconfig are
// refused; harmless ones (fetchinclude, access for the pinned endpoint)
// are not.
func TestG1_DisallowedLFSKeys(t *testing.T) {
	const ep = "https://h/x.git/info/lfs"
	list := strings.Join([]string{
		"lfs.url\nhttp://evil:8080/x",
		"lfs.pushurl\nhttp://evil/x",
		"remote.origin.lfsurl\nssh://git@evil/x",
		"remote.Upstream.lfspushurl\nhttp://evil/x",
		"lfs.http://evil/.access\nbasic",
		"lfs." + ep + ".access\nbasic",
		"lfs.customtransfer.x.path\ncalc",
		"lfs.standalonetransferagent\nx",
		"lfs.extension.foo.clean\nrm -rf /",
		"url.http://evil/.insteadof\nhttps://h/",
		"url.http://evil/.pushinsteadof\nhttps://h/",
		"lfs.fetchinclude\n*.bin",
		"lfs.access\nbasic",
		"core.bare", // key without a value
		"",
	}, "\x00")
	got := disallowedLFSKeys([]byte(list), ep)
	want := []string{"lfs.url", "lfs.pushurl", "remote.origin.lfsurl", "remote.Upstream.lfspushurl", "lfs.http://evil/.access",
		"lfs.customtransfer.x.path", "lfs.standalonetransferagent", "lfs.extension.foo.clean", "url.http://evil/.insteadof", "url.http://evil/.pushinsteadof"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := disallowedLFSKeys([]byte("LFS.URL\nx\x00"), ep); len(got) != 1 {
		t.Fatalf("case variant not refused: %q", got)
	}
}

// TestG2_AskpassExactPrompts: the helper answers only the exact prompts
// git prints for the configured host and user.
func TestG2_AskpassExactPrompts(t *testing.T) {
	type tc struct {
		prompt string
		want   string // "" means refused
	}
	const pass = plantedPassword
	run := func(t *testing.T, host, user string, cases []tc) {
		t.Helper()
		env := map[string]string{AskpassEnv: "1", askpassHostEnv: host, askpassUsernameEnv: user, askpassPasswordEnv: pass}
		for _, c := range cases {
			var out bytes.Buffer
			code, ok := Askpass([]string{"golem-deploy", c.prompt}, func(k string) string { return env[k] }, &out)
			switch {
			case !ok:
				t.Errorf("%q not handled", c.prompt)
			case c.want == "" && (code == 0 || out.Len() != 0):
				t.Errorf("host %s user %s: %q answered %q", host, user, c.prompt, out.String())
			case c.want != "" && (code != 0 || out.String() != c.want+"\n"):
				t.Errorf("host %s user %s: %q got code %d %q, want %q", host, user, c.prompt, code, out.String(), c.want)
			}
		}
	}

	t.Run("default port", func(t *testing.T) {
		run(t, "good.example", "x", []tc{
			{"Username for 'https://good.example': ", "x"},
			{"Username for 'https://good.example:443': ", "x"},
			{"Password for 'https://x@good.example': ", pass},
			{"Password for 'https://x@good.example:443': ", pass},
			{"Password for 'https://x@good.example': \n", pass}, // trailing newline trimmed
			// The reviewer's string: unpatched git talking to evil.example.
			{"Password for 'https://x@good.example/@evil.example': ", ""},
			{"Password for 'https://x%40good.example%2F@evil.example': ", ""},
			{"Password for 'https://x@good.example%2F@evil.example': ", ""},
			{"Password for 'https://x@evil.example': ", ""},
			{"Password for 'https://x@good.example.evil.example': ", ""},
			{"Password for 'https://x@good.example:8443': ", ""},
			{"Password for 'https://x@good.example:444': ", ""},
			{"Password for 'https://x@good.example/': ", ""},
			{"Password for 'https://x@good.example/path': ", ""},
			{"Password for 'https://x@good.example?q': ", ""},
			{"Password for 'https://x@good.example#f': ", ""},
			{"Password for 'https://y@good.example': ", ""},
			{"Password for 'https://x@y@good.example': ", ""},
			{"Password for 'http://x@good.example': ", ""},
			{`Password for "https://x@good.example": `, ""},
			{"Password for 'https://x@good.example':", ""},
			{"Password for 'https://x@good.example': \r\n", ""},
			{" Password for 'https://x@good.example': ", ""},
			{"Username for 'https://x@good.example': ", ""},
			{"Username for 'https://evil.example': ", ""},
			{"Password for 'https://good.example': ", ""},
			{"Are you sure you want to continue connecting (yes/no)? ", ""},
		})
	})
	t.Run("explicit 443", func(t *testing.T) {
		run(t, "good.example:443", "x", []tc{
			{"Password for 'https://x@good.example:443': ", pass},
			{"Password for 'https://x@good.example': ", pass},
			{"Username for 'https://good.example': ", "x"},
			{"Password for 'https://x@good.example:4430': ", ""},
		})
	})
	t.Run("other port", func(t *testing.T) {
		run(t, "Git.Example.com:8443", "x", []tc{
			{"Password for 'https://x@Git.Example.com:8443': ", pass},
			{"Password for 'https://x@git.example.com:8443': ", pass},
			{"Password for 'https://x@GIT.EXAMPLE.COM:8443': ", ""},
			{"Password for 'https://x@git.example.com': ", ""},
			{"Password for 'https://x@git.example.com:443': ", ""},
		})
	})
	t.Run("ipv6", func(t *testing.T) {
		run(t, "[::1]:8443", "x", []tc{
			{"Password for 'https://x@[::1]:8443': ", pass},
			{"Username for 'https://[::1]:8443': ", "x"},
			{"Password for 'https://x@[::1]': ", ""},
			{"Password for 'https://x@::1:8443': ", ""},
			{"Password for 'https://x@[::2]:8443': ", ""},
		})
		run(t, "[2001:db8::1]", "x", []tc{
			{"Password for 'https://x@[2001:db8::1]': ", pass},
			{"Password for 'https://x@[2001:db8::1]:443': ", pass},
			{"Password for 'https://x@[2001:DB8::1]': ", ""},
			{"Password for 'https://x@[2001:db8::1]:8443': ", ""},
		})
	})
	t.Run("percent-encoded user", func(t *testing.T) {
		// git >= 2.48.1 percent-encodes the user name in prompts.
		run(t, "good.example", "me@corp/team", []tc{
			{"Password for 'https://me@corp/team@good.example': ", pass},
			{"Password for 'https://me%40corp%2Fteam@good.example': ", pass},
			{"Password for 'https://me%40corp/team@good.example': ", pass},
			{"Password for 'https://me%40corp%2fteam@good.example': ", ""},
			{"Password for 'https://me@good.example': ", ""},
			{"Password for 'https://me@corp/team@evil.example': ", ""},
		})
	})
	t.Run("missing configuration", func(t *testing.T) {
		run(t, "", "x", []tc{{"Username for 'https://': ", ""}})
	})
}

// TestG3_SizeWatchdog: dirExceeds stops at the limit, and the watchdog
// fires once when a directory grows past it.
func TestG3_SizeWatchdog(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, n int) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a", 600)
	write("sub/b", 300)
	ctx := context.Background()
	if dirExceeds(ctx, dir, 1000) || !dirExceeds(ctx, dir, 899) {
		t.Fatal("dirExceeds wrong")
	}
	if dirExceeds(ctx, filepath.Join(dir, "missing"), 0) {
		t.Fatal("missing dir exceeds")
	}

	r := &Runner{maxSize: 2000, sizeInterval: 20 * time.Millisecond}
	var fired atomic.Int32
	done := make(chan struct{})
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer close(done)
		r.watchSize(wctx, dir, func() { fired.Add(1) })
	}()
	time.Sleep(100 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatal("fired below the limit")
	}
	write("sub/deep/c", 1500)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not fire")
	}
	if fired.Load() != 1 {
		t.Fatalf("fired %d times", fired.Load())
	}
}

// TestG7_SameOrigin: scheme kind, host (case-insensitive) and port with
// defaults made explicit.
func TestG7_SameOrigin(t *testing.T) {
	same := [][2]string{
		{"https://h.example/a.git", "https://h.example/b/c"},
		{"https://h.example/a", "https://H.EXAMPLE:443/a"},
		{"git@h.example:a.git", "ssh://git@h.example/a.git"},
		{"git@h.example:a.git", "ssh://other@h.example:22/b.git"},
		{"ssh://git@[::1]:2222/a", "ssh://git@[::1]:2222/b"},
		{"git@[::1]:a", "ssh://git@[::1]/a"},
	}
	diff := [][2]string{
		{"https://h.example/a", "https://evil.example/a"},
		{"https://h.example/a", "https://h.example:8443/a"},
		{"https://h.example/a", "git@h.example:a"},
		{"git@h.example:a", "ssh://git@h.example:2222/a"},
		{"https://h.example/a", "not a url"},
	}
	for _, c := range same {
		if !SameOrigin(c[0], c[1]) {
			t.Errorf("%q and %q should be the same origin", c[0], c[1])
		}
	}
	for _, c := range diff {
		if SameOrigin(c[0], c[1]) {
			t.Errorf("%q and %q should differ", c[0], c[1])
		}
	}
}
