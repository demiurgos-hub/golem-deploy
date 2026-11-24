package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// Image is an inspected managed image.
type Image struct {
	// ID is the immutable image id ("sha256:...").
	ID        string
	Tags      []string
	Labels    map[string]string
	SizeBytes int64
	Created   time.Time
	// ProjectID and Commit come from the build labels (empty until the
	// build pipeline sets them).
	ProjectID *int64
	Commit    string
	// Volumes are the paths the image declares with VOLUME, sorted.
	Volumes []string
}

type imageJSON struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Created  string   `json:"Created"`
	Size     int64    `json:"Size"`
	Config   struct {
		Labels  map[string]string   `json:"Labels"`
		Volumes map[string]struct{} `json:"Volumes"`
	} `json:"Config"`
}

func (j imageJSON) image() Image {
	labels := j.Config.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	tags := []string{}
	for _, t := range j.RepoTags {
		if t != "" && t != "<none>:<none>" {
			tags = append(tags, t)
		}
	}
	slices.Sort(tags)
	vols := make([]string, 0, len(j.Config.Volumes))
	for v := range j.Config.Volumes {
		vols = append(vols, v)
	}
	slices.Sort(vols)
	img := Image{ID: j.ID, Tags: tags, Labels: labels, SizeBytes: j.Size, Created: parseTime(j.Created), Commit: labels[LabelCommit], Volumes: vols}
	if id, err := strconv.ParseInt(labels[LabelProjectID], 10, 64); err == nil && id > 0 {
		img.ProjectID = &id
	}
	return img
}

func (c *Client) inspectImages(ctx context.Context, refs []string) ([]Image, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	args := append([]string{"image", "inspect", "--"}, refs...)
	out, err := c.output(ctx, quickTimeout, args...)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var raw []imageJSON
	if jerr := json.Unmarshal(out, &raw); jerr != nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("decode image inspect: %w", jerr)
	}
	if len(raw) == 0 && err != nil {
		return nil, err
	}
	list := make([]Image, 0, len(raw))
	for _, r := range raw {
		list = append(list, r.image())
	}
	return list, nil
}

// ListImages returns the managed images (deduplicated by id).
func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	out, err := c.output(ctx, quickTimeout, "image", "ls", "--all", "--no-trunc", "--filter", managedFilter(), "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	rows, err := decodeLines[struct {
		ID string `json:"ID"`
	}](out)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range rows {
		if r.ID != "" && !slices.Contains(ids, r.ID) {
			ids = append(ids, r.ID)
		}
	}
	all, err := c.inspectImages(ctx, ids)
	if err != nil {
		return nil, err
	}
	res := make([]Image, 0, len(all))
	for _, img := range all {
		if IsManaged(img.Labels) && !slices.ContainsFunc(res, func(x Image) bool { return x.ID == img.ID }) {
			res = append(res, img)
		}
	}
	slices.SortFunc(res, func(a, b Image) int { return b.Created.Compare(a.Created) })
	return res, nil
}

// ManagedImage resolves ref (tag, id or name@digest) and returns the image
// if it carries the managed label: ErrNotFound if absent, ErrNotManaged if
// unlabelled. Image trust is provisional: until the build pipeline records
// the ids of the images it builds, any image carrying the managed label is
// accepted (images are not labelled with the instance id).
func (c *Client) ManagedImage(ctx context.Context, ref string) (Image, error) {
	if err := ValidateImageRef(ref); err != nil {
		return Image{}, err
	}
	list, err := c.inspectImages(ctx, []string{ref})
	if err != nil {
		return Image{}, err
	}
	if len(list) != 1 {
		return Image{}, ErrNotFound
	}
	if !IsManaged(list[0].Labels) || !imageIDRE.MatchString(list[0].ID) {
		return Image{}, ErrNotManaged
	}
	return list[0], nil
}

// ContainersUsingImage reports whether any container (managed or not) was
// created from the image id.
func (c *Client) containersUsingImage(ctx context.Context, id string) (bool, error) {
	ids, err := c.psIDs(ctx, "ancestor="+id)
	if err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}

// RemoveImage deletes a managed image. It refuses unmanaged images
// (ErrNotManaged) and images used by any container (ErrInUse); callers
// check their own references (server rows) first.
func (c *Client) RemoveImage(ctx context.Context, id string) error {
	if err := ValidateImageID(id); err != nil {
		return err
	}
	img, err := c.ManagedImage(ctx, id)
	if err != nil {
		return err
	}
	used, err := c.containersUsingImage(ctx, img.ID)
	if err != nil {
		return err
	}
	if used {
		return ErrInUse
	}
	// Always by the immutable id that was checked, never by a tag (a tag
	// can be moved to another image in between). An image with several
	// tags needs --force to be removed by id; --force never removes an
	// image a running container uses, and no container used it a moment
	// ago.
	args := []string{"image", "rm"}
	if len(img.Tags) > 1 {
		args = append(args, "--force")
	}
	args = append(args, "--", img.ID)
	c.logger.Info("docker image rm", "image", img.ID, "tags", len(img.Tags))
	if err := c.exec(ctx, quickTimeout, args, nil, nil, nil); err != nil {
		if errors.Is(err, ErrConflict) {
			return ErrInUse
		}
		return err
	}
	return nil
}
