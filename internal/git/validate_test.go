package git

import (
	"strings"
	"testing"
)

func TestValidateRemoteURL(t *testing.T) {
	ok := map[string]Kind{
		"https://github.com/golang/example":           KindHTTPS,
		"https://github.com/golang/example.git":       KindHTTPS,
		"HTTPS://GitHub.com/x/y":                      KindHTTPS,
		"https://git.example.com:8443/team/game.git":  KindHTTPS,
		"https://127.0.0.1:3000/git/repo.git":         KindHTTPS,
		"https://[::1]:3000/repo.git":                 KindHTTPS,
		"https://host/a%20b.git":                      KindHTTPS,
		"ssh://git@github.com/org/repo.git":           KindSSH,
		"ssh://github.com/org/repo.git":               KindSSH,
		"ssh://git@git.example.com:2222/org/repo.git": KindSSH,
		"git@github.com:org/repo.git":                 KindSSH,
		"deploy_user@host.example:repo":               KindSSH,
		"git@[2001:db8::1]:org/repo.git":              KindSSH,
	}
	for in, want := range ok {
		got, err := ValidateRemoteURL(in)
		if err != nil || got != want {
			t.Errorf("ValidateRemoteURL(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	bad := []string{
		"",
		"--upload-pack=touch /tmp/pwned",
		"--upload-pack=touch",
		"-oProxyCommand=calc",
		"ssh://-oProxyCommand=calc/x",
		"ssh://git@-oProxyCommand=calc/x",
		"git@-oProxyCommand=calc:x",
		"-git@host:x",
		"git@host:-x",
		"ext::sh -c touch% /tmp/pwned",
		"ext::sh",
		"fd::17",
		"file:///etc",
		"file:///etc/passwd",
		"FILE:///etc",
		"/etc/passwd",
		"../repo",
		"./repo",
		"repo",
		"C:/Users/x/repo",
		"C:\\Users\\x\\repo",
		"\\\\server\\share\\repo",
		"http://github.com/x/y",
		"git://github.com/x/y",
		"git+ssh://git@github.com/x/y",
		"ftp://host/x",
		"https://user:pass@host/x",
		"https://user@host/x",
		"https://:pass@host/x",
		"ssh://user:pass@host/x",
		"https://host",
		"https://host/",
		"https:///x",
		"https://host/x?upload-pack=y",
		"https://host/x#frag",
		"https://host:0/x",
		"https://host:99999/x",
		"https://host:/x",
		"https://ho_st/x",
		"https://host..example/x",
		"https://host/x y",
		"https://host/x\ty",
		"https://host/x\x00",
		"https://host/x\n",
		"https://host/x\u200b",
		"https://host/" + strings.Repeat("a", MaxURLLength),
		"git@host:",
		"git@host:/",
		"@host:x",
		"git@:x",
		"host:x",
		"git@host/x:y",
		"ss h://host/x",
	}
	for _, in := range bad {
		if k, err := ValidateRemoteURL(in); err == nil {
			t.Errorf("ValidateRemoteURL(%q) accepted as %v", in, k)
		} else if in != "" && len(in) > 8 && strings.Contains(err.Error(), in) {
			t.Errorf("error for %q echoes the input: %v", in, err)
		}
	}
}

func TestValidateBranch(t *testing.T) {
	for _, b := range []string{"main", "master", "feature/x", "release-1.2", "v1.0", "user/ünï", "a.b/c_d", "x@y", strings.Repeat("a", MaxBranchLength)} {
		if err := ValidateBranch(b); err != nil {
			t.Errorf("ValidateBranch(%q): %v", b, err)
		}
	}
	for _, b := range []string{
		"", "-x", "--output=/etc/passwd", "--upload-pack=x", "HEAD", "@",
		"a..b", "..", "a/../b", "a@{1}", "@{-1}", "/a", "a/", "a//b", "a.", ".a", "a/.b", "a.lock", "a/b.lock/c",
		"a b", "a\tb", "a\nb", "a\x00b", "a\x7fb", "a~1", "a^", "a:b", "a?", "a*", "a[b", "a\\b",
		strings.Repeat("a", MaxBranchLength+1), "a\u200bb", "\xff",
	} {
		if err := ValidateBranch(b); err == nil {
			t.Errorf("ValidateBranch(%q) accepted", b)
		}
	}
}

func TestRemoteValidateAuthKind(t *testing.T) {
	cases := []struct {
		r  Remote
		ok bool
	}{
		{Remote{URL: "https://h/x", Auth: Auth{Method: AuthHTTPS, Username: "u", Password: "p"}}, true},
		{Remote{URL: "git@h:x", Auth: Auth{Method: AuthHTTPS, Username: "u", Password: "p"}}, false},
		{Remote{URL: "https://h/x", Auth: Auth{Method: AuthSSH, PrivateKey: []byte("k")}}, false},
		{Remote{URL: "ssh://h/x", Auth: Auth{Method: AuthSSH, PrivateKey: []byte("k")}}, true},
		{Remote{URL: "ssh://h/x", Auth: Auth{Method: AuthSSH}}, false},
		{Remote{URL: "https://h/x", Auth: Auth{Method: AuthHTTPS, Username: "u"}}, false},
		{Remote{URL: "https://h/x", Auth: Auth{Method: AuthHTTPS, Username: "u", Password: "a\nb"}}, false},
		{Remote{URL: "https://h/x", Branch: "--output=x"}, false},
		{Remote{URL: "https://h/x", Branch: "main"}, true},
	}
	for i, c := range cases {
		if _, err := c.r.Validate(); (err == nil) != c.ok {
			t.Errorf("case %d: err %v, want ok=%v", i, err, c.ok)
		}
	}
}

func TestURLHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/x":     "github.com",
		"https://h.example:8443/x": "h.example:8443",
		"ssh://git@h:22/x":         "h:22",
		"git@github.com:org/repo":  "",
		"https://[::1]:3000/repo":  "[::1]:3000",
	} {
		if got := URLHost(in); got != want {
			t.Errorf("URLHost(%q) = %q, want %q", in, got, want)
		}
	}
}
