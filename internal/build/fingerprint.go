package build

import (
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// HelpersFile is the generated Go file that declares the game's schema
// fingerprint, in the go-server integration's output directory.
const HelpersFile = "golem_helpers.go"

// maxHelpersBytes caps the generated file read for the fingerprint.
const maxHelpersBytes = 1 << 20

// fingerprintDeclRE matches the declaration golem-bake writes, alone on
// its line.
var fingerprintDeclRE = regexp.MustCompile(`(?m)^[ \t]*const[ \t]+SchemaFingerprint[ \t]*=[ \t]*"([0-9a-f]{64})"[ \t]*\r?$`)

// committedFingerprint reads the schema fingerprint from the committed
// generated Go code: the SchemaFingerprint constant in golem_helpers.go
// inside the directory integrations.go-server.out of golem.yaml names. The
// build verifies that code with golem-bake --check, so a successful build
// carries the fingerprint the game reports. Anything unexpected (no such
// integration, an unsafe path, a link, a missing or large file, no or more
// than one declaration) gives "": the fingerprint is then unknown until a
// server reports it, and the build is not affected.
func committedFingerprint(repo *repoFS, root string, golemYAML []byte) string {
	if checkYAML("golem.yaml", golemYAML) != nil {
		return ""
	}
	var gy struct {
		Integrations map[string]struct {
			Out string `yaml:"out"`
		} `yaml:"integrations"`
	}
	if yaml.Unmarshal(golemYAML, &gy) != nil {
		return ""
	}
	in, ok := gy.Integrations["go-server"]
	if !ok {
		return ""
	}
	out := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(in.Out), "./"), "/")
	if out == "" || path.Clean(out) != out || out == "." || out == ".." || strings.HasPrefix(out, "/") || strings.HasPrefix(out, "../") ||
		strings.ContainsAny(out, "\\:") {
		return ""
	}
	if _, err := cleanPath("out", out, false); err != nil {
		return ""
	}
	data, err := repo.readFile(join(root, out, HelpersFile), maxHelpersBytes)
	if err != nil {
		return ""
	}
	return parseFingerprintDecl(data)
}

// parseFingerprintDecl returns the fingerprint of the single
// SchemaFingerprint declaration in a generated file ("" unless there is
// exactly one).
func parseFingerprintDecl(data []byte) string {
	m := fingerprintDeclRE.FindAllSubmatch(data, 2)
	if len(m) != 1 {
		return ""
	}
	return string(m[0][1])
}
