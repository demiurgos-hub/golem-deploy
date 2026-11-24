package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Volume is an inspected managed volume.
type Volume struct {
	Name       string
	Labels     map[string]string
	Created    time.Time
	ServerID   int64 // 0 when not labelled with a server
	ServerName string
	// InUse reports whether any container (managed or not) mounts it.
	InUse bool
}

type volumeJSON struct {
	Name      string            `json:"Name"`
	CreatedAt string            `json:"CreatedAt"`
	Labels    map[string]string `json:"Labels"`
}

func (c *Client) inspectVolumes(ctx context.Context, names []string) ([]Volume, error) {
	if len(names) == 0 {
		return nil, nil
	}
	args := append([]string{"volume", "inspect", "--"}, names...)
	out, err := c.output(ctx, quickTimeout, args...)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var raw []volumeJSON
	if jerr := json.Unmarshal(out, &raw); jerr != nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("decode volume inspect: %w", jerr)
	}
	if len(raw) == 0 && err != nil {
		return nil, err
	}
	list := make([]Volume, 0, len(raw))
	for _, r := range raw {
		labels := r.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		list = append(list, Volume{
			Name:       r.Name,
			Labels:     labels,
			Created:    parseTime(r.CreatedAt),
			ServerID:   serverIDLabel(labels),
			ServerName: labels[LabelServer],
		})
	}
	return list, nil
}

// mountedVolumes returns the names of volumes mounted by any container.
func (c *Client) mountedVolumes(ctx context.Context, filters ...string) (map[string]bool, error) {
	args := []string{"ps", "--all", "--no-trunc"}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	args = append(args, "--format", "{{json .}}")
	out, err := c.output(ctx, quickTimeout, args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeLines[struct {
		Mounts string `json:"Mounts"`
	}](out)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, r := range rows {
		for _, m := range strings.Split(r.Mounts, ",") {
			if m = strings.TrimSpace(m); m != "" {
				used[m] = true
			}
		}
	}
	return used, nil
}

// ListVolumes returns the volumes carrying the managed label and this
// installation's instance id, with their attachment state.
func (c *Client) ListVolumes(ctx context.Context) ([]Volume, error) {
	if err := c.needInstance(); err != nil {
		return nil, err
	}
	out, err := c.output(ctx, quickTimeout, "volume", "ls", "--filter", managedFilter(), "--filter", c.instanceFilter(), "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	rows, err := decodeLines[struct {
		Name string `json:"Name"`
	}](out)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range rows {
		if r.Name != "" {
			names = append(names, r.Name)
		}
	}
	all, err := c.inspectVolumes(ctx, names)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	used, err := c.mountedVolumes(ctx)
	if err != nil {
		return nil, err
	}
	res := make([]Volume, 0, len(all))
	for _, v := range all {
		if c.ours(v.Labels) {
			v.InUse = used[v.Name]
			res = append(res, v)
		}
	}
	slices.SortFunc(res, func(a, b Volume) int { return strings.Compare(a.Name, b.Name) })
	return res, nil
}

// RemoveVolume deletes a volume of this installation. It refuses
// unmanaged volumes (ErrNotManaged), managed volumes of another
// installation (ErrNotOurs) and volumes mounted by any container (ErrInUse).
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	if err := c.needInstance(); err != nil {
		return err
	}
	if err := ValidateVolumeName(name); err != nil {
		return err
	}
	list, err := c.inspectVolumes(ctx, []string{name})
	if err != nil {
		return err
	}
	if len(list) != 1 || list[0].Name != name {
		return ErrNotFound
	}
	if !IsManaged(list[0].Labels) {
		return ErrNotManaged
	}
	if !c.ours(list[0].Labels) {
		return ErrNotOurs
	}
	used, err := c.mountedVolumes(ctx, "volume="+name)
	if err != nil {
		return err
	}
	if len(used) > 0 {
		return ErrInUse
	}
	c.logger.Info("docker volume rm", "volume", name)
	if err := c.exec(ctx, quickTimeout, []string{"volume", "rm", "--", name}, nil, nil, nil); err != nil {
		if errors.Is(err, ErrConflict) {
			return ErrInUse
		}
		return err
	}
	return nil
}
