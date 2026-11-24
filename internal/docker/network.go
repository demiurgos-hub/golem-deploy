package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// NetworkICCOption is the bridge driver option that controls traffic
// between containers on the network; the managed network sets it to false.
const NetworkICCOption = "com.docker.network.bridge.enable_icc"

type networkInfo struct {
	Labels map[string]string
	// ICC is false only when the network was created with
	// NetworkICCOption=false.
	ICC bool
	// Attached counts containers connected to the network.
	Attached int
}

// inspectNetwork returns the named network, or ErrNotFound.
func (c *Client) inspectNetwork(ctx context.Context, name string) (networkInfo, error) {
	out, err := c.output(ctx, quickTimeout, "network", "inspect", "--", name)
	if err != nil {
		return networkInfo{}, err
	}
	var raw []struct {
		Name       string            `json:"Name"`
		Labels     map[string]string `json:"Labels"`
		Options    map[string]string `json:"Options"`
		Containers map[string]any    `json:"Containers"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return networkInfo{}, fmt.Errorf("decode network inspect: %w", err)
	}
	if len(raw) != 1 {
		return networkInfo{}, ErrNotFound
	}
	n := networkInfo{Labels: raw[0].Labels, ICC: raw[0].Options[NetworkICCOption] != "false", Attached: len(raw[0].Containers)}
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	return n, nil
}

func (c *Client) createNetwork(ctx context.Context) error {
	c.logger.Info("docker network create", "network", NetworkName)
	return c.exec(ctx, quickTimeout, []string{"network", "create", "--driver", "bridge",
		"--opt", NetworkICCOption + "=false",
		"--label", LabelManaged + "=" + ManagedValue,
		"--label", LabelInstance + "=" + c.instance,
		"--", NetworkName}, nil, nil, nil)
}

// EnsureNetwork makes sure the managed bridge network NetworkName exists,
// belongs to this installation and isolates containers from each other
// (inter-container communication disabled). It returns isolated=false when
// our network still allows inter-container traffic but cannot be recreated
// because containers are attached (isolation then applies once they are
// gone and the network is recreated on a later start).
//
// A network of that name without the managed label is refused
// (ErrNotManaged). A managed network of another installation is refused
// (ErrNotOurs) and never changed; one without any instance label (created
// by an older version) is removed and recreated when nothing is attached,
// and refused otherwise.
func (c *Client) EnsureNetwork(ctx context.Context) (isolated bool, err error) {
	if err := c.needInstance(); err != nil {
		return false, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		n, err := c.inspectNetwork(ctx, NetworkName)
		switch {
		case errors.Is(err, ErrNotFound):
			err = c.createNetwork(ctx)
			if err == nil {
				return true, nil
			}
			if !errors.Is(err, ErrConflict) {
				return false, err
			}
			continue // created concurrently; inspect again
		case err != nil:
			return false, err
		case !IsManaged(n.Labels):
			return false, fmt.Errorf("network %q: %w", NetworkName, ErrNotManaged)
		}
		legacy := n.Labels[LabelInstance] == ""
		if !legacy && !c.ours(n.Labels) {
			return false, fmt.Errorf("network %q: %w", NetworkName, ErrNotOurs)
		}
		if !legacy && !n.ICC {
			return true, nil
		}
		if n.Attached > 0 {
			if legacy {
				return false, fmt.Errorf("network %q (created by an older version, containers attached): %w", NetworkName, ErrNotOurs)
			}
			c.logger.Warn("the "+NetworkName+" network allows traffic between containers and still has containers attached; "+
				"isolation applies after every server is stopped and the daemon restarted", "attached", n.Attached)
			return false, nil
		}
		c.logger.Info("docker network rm (recreating without inter-container traffic)", "network", NetworkName, "legacy", legacy)
		if err := c.exec(ctx, quickTimeout, []string{"network", "rm", "--", NetworkName}, nil, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
			return false, err
		}
	}
	return false, fmt.Errorf("network %q: %w", NetworkName, ErrConflict)
}
