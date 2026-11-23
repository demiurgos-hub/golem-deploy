package docker

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateServerName(t *testing.T) {
	for name, ok := range map[string]bool{
		"a": true, "game-1": true, "0x": true, strings.Repeat("a", 63): true,
		"": false, "-a": false, "A": false, "a_b": false, "a.b": false, "a b": false,
		strings.Repeat("a", 64): false, "../x": false, "a\n": false,
	} {
		if err := ValidateServerName(name); (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", name, err, ok)
		}
	}
}

func TestValidateEnvKey(t *testing.T) {
	for key, ok := range map[string]bool{
		"A": true, "_x": true, "GAME_MODE": true, "a1": true, strings.Repeat("A", 128): true,
		"": false, "1A": false, "A-B": false, "A=B": false, "A B": false, strings.Repeat("A", 129): false,
		// Reserved: they would steer the docker CLI child itself.
		"PATH": false, "Path": false, "HOME": false, "DOCKER_HOST": false, "docker_context": false,
		"DOCKER_ANYTHING": false, "HTTPS_PROXY": false, "SystemRoot": false, "USERPROFILE": false,
	} {
		if err := ValidateEnvKey(key); (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", key, err, ok)
		}
	}
}

func TestValidateEnv(t *testing.T) {
	const secret = "s3cr3t-marker"
	cases := []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"empty", nil, true},
		{"normal", map[string]string{"A": secret, "B": ""}, true},
		{"newline value ok", map[string]string{"A": "x\ny"}, true},
		{"nul value", map[string]string{"A": "x\x00" + secret}, false},
		{"bad key", map[string]string{"1A": secret}, false},
		{"case duplicate", map[string]string{"Mode": "a", "MODE": "b"}, false},
		{"too large", map[string]string{"A": strings.Repeat("x", MaxEnvBytes)}, false},
	}
	for _, c := range cases {
		err := ValidateEnv(c.env)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks the value: %v", c.name, err)
		}
		var ve *ValidationError
		if err != nil && !errors.As(err, &ve) {
			t.Errorf("%s: not a ValidationError: %T", c.name, err)
		}
	}
	many := map[string]string{}
	for i := 0; i <= MaxEnvVars; i++ {
		many["K"+strings.Repeat("A", i%100)+string(rune('A'+i%26))+string(rune('A'+i/26))] = ""
	}
	if err := ValidateEnv(many); err == nil {
		t.Error("too many variables accepted")
	}
}

func TestValidateRefs(t *testing.T) {
	for ref, ok := range map[string]bool{
		"game:latest": true, "registry.local/team/game:v1": true, "sha256:" + strings.Repeat("a", 64): true,
		"game@sha256:" + strings.Repeat("b", 64): true, "": false, "-rm": false, "--privileged": false, "a b": false, "a\nb": false, "../x": false,
	} {
		if err := ValidateImageRef(ref); (err == nil) != ok {
			t.Errorf("image ref %q: err=%v, want ok=%v", ref, err, ok)
		}
	}
	for id, ok := range map[string]bool{
		"sha256:" + strings.Repeat("a", 64): true, strings.Repeat("a", 12): true,
		"game:latest": false, "-f": false, "sha256:xyz": false, strings.Repeat("a", 11): false,
	} {
		if err := ValidateImageID(id); (err == nil) != ok {
			t.Errorf("image id %q: err=%v, want ok=%v", id, err, ok)
		}
	}
	for name, ok := range map[string]bool{
		"golem-deploy-a-data": true, "v1": true, "a.b_c": true,
		"": false, "-v": false, "/etc": false, "a/b": false, "a b": false,
	} {
		if err := ValidateVolumeName(name); (err == nil) != ok {
			t.Errorf("volume %q: err=%v, want ok=%v", name, err, ok)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"0B": 0, "1.5kB": 1500, "2MB": 2_000_000, "1KiB": 1024, "12MiB": 12 << 20, "1.5GiB": 3 << 29, "--": 0, "": 0, "7": 7,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"x", "1XB", "1..2MB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	s, err := parseStats(statsJSON{CPUPerc: "5.25%", MemUsage: "326.1MiB / 15.34GiB", NetIO: "6.72MB / 5.12MB", PIDs: "29"})
	if err != nil || s.CPUPercent != 5.25 || s.PIDs != 29 || s.NetworkRxBytes != 6_720_000 || s.MemoryLimitBytes == 0 {
		t.Fatalf("%+v %v", s, err)
	}
}
