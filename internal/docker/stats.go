package docker

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Stats is one resource usage sample of a running container.
type Stats struct {
	CPUPercent       float64
	MemoryBytes      int64
	MemoryLimitBytes int64
	NetworkRxBytes   int64
	NetworkTxBytes   int64
	PIDs             int
}

type statsJSON struct {
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
	NetIO    string `json:"NetIO"`
	PIDs     string `json:"PIDs"`
}

// ServerStats samples the server's running container once.
func (c *Client) ServerStats(ctx context.Context, ref ServerRef) (Stats, error) {
	ct, err := c.ServerContainer(ctx, ref)
	if err != nil {
		return Stats{}, err
	}
	if !ct.Running {
		return Stats{}, ErrNotRunning
	}
	out, err := c.output(ctx, quickTimeout, "stats", "--no-stream", "--no-trunc", "--format", "{{json .}}", "--", ct.ID)
	if err != nil {
		return Stats{}, err
	}
	rows, err := decodeLines[statsJSON](out)
	if err != nil {
		return Stats{}, err
	}
	if len(rows) != 1 {
		return Stats{}, fmt.Errorf("docker stats: expected one row, got %d", len(rows))
	}
	return parseStats(rows[0])
}

func parseStats(r statsJSON) (Stats, error) {
	var s Stats
	var err error
	if s.CPUPercent, err = parsePercent(r.CPUPerc); err != nil {
		return Stats{}, err
	}
	if s.MemoryBytes, s.MemoryLimitBytes, err = parsePair(r.MemUsage); err != nil {
		return Stats{}, err
	}
	if s.NetworkRxBytes, s.NetworkTxBytes, err = parsePair(r.NetIO); err != nil {
		return Stats{}, err
	}
	if p := strings.TrimSpace(r.PIDs); p != "" && p != "--" {
		if s.PIDs, err = strconv.Atoi(p); err != nil {
			return Stats{}, fmt.Errorf("parse pids %q", r.PIDs)
		}
	}
	return s, nil
}

func parsePercent(v string) (float64, error) {
	v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "%"))
	if v == "" || v == "--" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("parse percentage %q", v)
	}
	return f, nil
}

// parsePair parses "a / b" where both are human-readable sizes.
func parsePair(v string) (int64, int64, error) {
	a, b, ok := strings.Cut(v, "/")
	if !ok {
		if strings.TrimSpace(v) == "" || strings.TrimSpace(v) == "--" {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("parse size pair %q", v)
	}
	x, err := ParseSize(a)
	if err != nil {
		return 0, 0, err
	}
	y, err := ParseSize(b)
	if err != nil {
		return 0, 0, err
	}
	return x, y, nil
}

// ParseSize parses Docker's human-readable sizes, both decimal ("1.5kB",
// "3MB") and binary ("1.5KiB", "3MiB"), into bytes.
func ParseSize(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "--" {
		return 0, nil
	}
	i := 0
	for i < len(v) && (v[i] >= '0' && v[i] <= '9' || v[i] == '.') {
		i++
	}
	num, unit := v[:i], strings.TrimSpace(v[i:])
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q", v)
	}
	mult := map[string]float64{
		"": 1, "B": 1,
		"kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "PiB": 1 << 50,
	}[unit]
	if mult == 0 {
		return 0, fmt.Errorf("parse size %q: unknown unit", v)
	}
	b := f * mult
	if b > math.MaxInt64 {
		return 0, fmt.Errorf("parse size %q: too large", v)
	}
	return int64(b), nil
}
