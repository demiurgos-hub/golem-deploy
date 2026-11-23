package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// networkLabels returns the labels of the named network, or ErrNotFound.
func (c *Client) networkLabels(ctx context.Context, name string) (map[string]string, error) {
	out, err := c.output(ctx, quickTimeout, "network", "inspect", "--", name)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decode network inspect: %w", err)
	}
	if len(raw) != 1 {
		return nil, ErrNotFound
	}
	if raw[0].Labels == nil {
		return map[string]string{}, nil
	}
	return raw[0].Labels, nil
}

// EnsureNetwork creates the managed bridge network NetworkName if it does
// not exist. An existing network of that name without the managed label is
// refused (ErrNotManaged) rather than joined.
func (c *Client) EnsureNetwork(ctx context.Context) error {
	for attempt := 0; attempt < 2; attempt++ {
		labels, err := c.networkLabels(ctx, NetworkName)
		switch {
		case err == nil && IsManaged(labels):
			return nil
		case err == nil:
			return fmt.Errorf("network %q: %w", NetworkName, ErrNotManaged)
		case !errors.Is(err, ErrNotFound):
			return err
		}
		c.logger.Info("docker network create", "network", NetworkName)
		err = c.exec(ctx, quickTimeout, []string{"network", "create", "--driver", "bridge",
			"--label", LabelManaged + "=" + ManagedValue, "--", NetworkName}, nil, nil, nil)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		// Created concurrently; inspect again.
	}
	return fmt.Errorf("network %q: %w", NetworkName, ErrConflict)
}
