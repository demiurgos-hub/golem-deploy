package build

import (
	"bytes"
	"errors"
	"io"

	"go.yaml.in/yaml/v3"
)

// Decode into nodes first. Typed decoding performs pairwise duplicate-key
// checks and resolves aliases, including in data the daemon does not use.
func checkYAML(name string, data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return userErr("%s is not valid YAML: %s", name, yamlMessage(err))
	}
	remaining := 1024
	var check func(*yaml.Node, int) bool
	check = func(n *yaml.Node, depth int) bool {
		remaining--
		if remaining < 0 || depth > 32 || (n.Kind == yaml.MappingNode && len(n.Content) > 256) {
			return false
		}
		if n.Kind == yaml.AliasNode {
			return false
		}
		for _, child := range n.Content {
			if !check(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !check(&doc, 0) {
		return userErr("%s exceeds the YAML complexity limits (1024 nodes, 32 levels, 128 keys per mapping, no aliases)", name)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return userErr("%s must contain a single YAML document", name)
	}
	return nil
}
