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
	// ProjectID, Commit and BuildID come from the build labels (empty or
	// zero when absent).
	ProjectID *int64
	Commit    string
	BuildID   int64
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
	if id, err := strconv.ParseInt(labels[LabelBuildID], 10, 64); err == nil && id > 0 {
		img.BuildID = id
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

// ListImages returns the images this installation manages (managed and
// instance labels; deduplicated by id).
func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	if err := c.needInstance(); err != nil {
		return nil, err
	}
	out, err := c.output(ctx, quickTimeout, "image", "ls", "--all", "--no-trunc", "--filter", managedFilter(), "--filter", c.instanceFilter(),
		"--format", "{{json .}}")
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
		if c.ours(img.Labels) && !slices.ContainsFunc(res, func(x Image) bool { return x.ID == img.ID }) {
			res = append(res, img)
		}
	}
	slices.SortFunc(res, func(a, b Image) int { return b.Created.Compare(a.Created) })
	return res, nil
}

// ManagedImage resolves ref (tag, id or name@digest) and returns the image
// if it carries the managed label and this installation's instance label:
// ErrNotFound if absent, ErrNotManaged otherwise. It is used to list and
// delete images; it is not enough to run one: labels can be set by anyone
// who builds an image, so a server runs only the image recorded on a
// successful build, verified with BuildImage.
func (c *Client) ManagedImage(ctx context.Context, ref string) (Image, error) {
	if err := c.needInstance(); err != nil {
		return Image{}, err
	}
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
	if !c.ours(list[0].Labels) || !imageIDRE.MatchString(list[0].ID) {
		return Image{}, ErrNotManaged
	}
	return list[0], nil
}

// BuildImage verifies the image of a successful build before a server
// runs it: ref.ImageID (a full immutable id, the image_id of a success row
// of the builds table) must exist and carry the managed label, this
// installation's instance label and exactly the build labels the build
// pipeline set (build id, project id, commit, engine version; no other
// label in the namespace), the same check BuiltImage made when the build
// finished. It returns ErrNotFound when the image is gone, ErrNotManaged
// when the managed or instance label is missing or different, and
// ErrNotOurs when the build labels differ. The store row is what makes an
// image trusted; the labels are a second check that the id still names the
// image that build produced.
func (c *Client) BuildImage(ctx context.Context, ref BuildRef) (Image, error) {
	if err := c.needInstance(); err != nil {
		return Image{}, err
	}
	if !imageIDRE.MatchString(ref.ImageID) || ref.BuildID <= 0 {
		return Image{}, invalid("image", "image must be a full image id of a build")
	}
	list, err := c.inspectImages(ctx, []string{ref.ImageID})
	if err != nil {
		return Image{}, err
	}
	if len(list) != 1 || list[0].ID != ref.ImageID {
		return Image{}, ErrNotFound
	}
	img := list[0]
	if !c.ours(img.Labels) {
		return Image{}, ErrNotManaged
	}
	if !matchBuildLabels(img.Labels, ref.Labels(c.instance)) {
		return Image{}, ErrNotOurs
	}
	return img, nil
}

// ErrImageMultipleTags means the operator needs to untag aliases before
// the immutable image id can be removed safely.
var ErrImageMultipleTags = errors.New("remove additional tags from the image before deleting it")

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
	used, err := c.ContainersUsingImage(ctx, img.ID)
	if err != nil {
		return err
	}
	if len(used) > 0 {
		return ErrInUse
	}
	if len(img.Tags) > 1 {
		return ErrImageMultipleTags
	}
	// Use the checked immutable id without forcing removal: Docker must
	// also protect references created after our container check.
	args := []string{"image", "rm", "--", img.ID}
	c.logger.Info("docker image rm", "image", img.ID, "tags", len(img.Tags))
	if err := c.exec(ctx, quickTimeout, args, nil, nil, nil); err != nil {
		if errors.Is(err, ErrConflict) {
			return ErrInUse
		}
		return err
	}
	return nil
}
