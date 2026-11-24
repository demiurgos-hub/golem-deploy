package dockertest

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ParseEnvFile parses env-file content exactly like the docker CLI does
// (github.com/docker/cli pkg/kvfile, used by "docker run --env-file"):
// lines are read with a bufio.Scanner (64 KiB limit, a trailing "\r" is
// dropped), a UTF-8 BOM is trimmed from the first line, leading whitespace
// is trimmed, empty lines and lines starting with "#" are skipped, the key
// ends at the first "=" and the value is the rest of the line, taken
// literally. A line without "=" copies the variable from lookup (the CLI's
// own process environment) when it is set there. Invalid UTF-8 and keys with
// whitespace are errors.
func ParseEnvFile(content []byte, lookup func(string) (string, bool)) ([]string, error) {
	const whiteSpaces = " \t"
	lines := []string{}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	utf8bom := []byte{0xEF, 0xBB, 0xBF}
	for currentLine := 1; scanner.Scan(); currentLine++ {
		scanned := scanner.Bytes()
		if !utf8.Valid(scanned) {
			return nil, fmt.Errorf("invalid utf8 bytes at line %d", currentLine)
		}
		if currentLine == 1 {
			scanned = bytes.TrimPrefix(scanned, utf8bom)
		}
		line := strings.TrimLeftFunc(string(scanned), unicode.IsSpace)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		key, _, hasValue := strings.Cut(line, "=")
		if len(key) == 0 {
			return nil, fmt.Errorf("no variable name on line %d", currentLine)
		}
		if strings.ContainsAny(key, whiteSpaces) {
			return nil, fmt.Errorf("variable on line %d contains whitespaces", currentLine)
		}
		if hasValue {
			lines = append(lines, line)
			continue
		}
		if lookup != nil {
			if value, found := lookup(line); found {
				lines = append(lines, key+"="+value)
			}
		}
	}
	return lines, scanner.Err()
}
