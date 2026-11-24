package servers

import (
	"context"
	"errors"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
)

// Images returns this installation's managed images with the builds that
// produced them and the servers whose build uses them. An image is in_use
// while a successful build records it or any container (managed or not,
// running or stopped) was created from it: the rule every deletion path
// applies (DELETE /images here, build deletion, retention and project
// deletion in internal/build).
func (s *Service) Images(ctx context.Context) ([]apitypes.Image, error) {
	imgs, err := s.docker.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	byImage, err := s.store.BuildsByImage(ctx)
	if err != nil {
		return nil, err
	}
	usedByContainers, err := s.docker.ImagesInUse(ctx)
	if err != nil {
		return nil, err
	}
	bc := s.builds()
	serverImage := map[int64]string{}
	for _, r := range rows {
		b, err := bc.get(ctx, r)
		if err != nil {
			return nil, err
		}
		if b != nil {
			serverImage[r.ID] = b.ImageID
		}
	}
	out := make([]apitypes.Image, 0, len(imgs))
	for _, img := range imgs {
		v := apitypes.Image{
			ID:        img.ID,
			Tags:      img.Tags,
			ProjectID: img.ProjectID,
			Commit:    img.Commit,
			SizeBytes: img.SizeBytes,
			CreatedAt: img.Created,
			ServerIDs: []int64{},
			BuildIDs:  append([]int64{}, byImage[img.ID]...),
		}
		for _, r := range rows {
			if serverImage[r.ID] == img.ID {
				v.ServerIDs = append(v.ServerIDs, r.ID)
			}
		}
		v.InUse = len(v.ServerIDs) > 0 || len(v.BuildIDs) > 0 || usedByContainers[img.ID]
		out = append(out, v)
	}
	return out, nil
}

// ImageOfBuildError: the image is recorded on successful builds, so it is
// removed by deleting them (DELETE /builds/{id}), never through /images. It
// matches ErrInUse.
type ImageOfBuildError struct {
	BuildIDs []int64
}

func (e *ImageOfBuildError) Error() string { return ErrInUse.Error() }

// Is makes errors.Is(err, ErrInUse) true.
func (e *ImageOfBuildError) Is(target error) bool { return target == ErrInUse }

// RemoveImage deletes a managed image that no successful build records
// (delete the build instead) and no container uses. Unknown and unmanaged
// images are both ErrNotFound so the API does not reveal unmanaged images.
func (s *Service) RemoveImage(ctx context.Context, id string) error {
	if err := docker.ValidateImageID(id); err != nil {
		return fieldErr("id", "invalid image id")
	}
	img, err := s.docker.ManagedImage(ctx, id)
	switch {
	case errors.Is(err, docker.ErrNotFound), errors.Is(err, docker.ErrNotManaged):
		return ErrNotFound
	case err != nil:
		return err
	}
	byImage, err := s.store.BuildsByImage(ctx)
	if err != nil {
		return err
	}
	if ids := byImage[img.ID]; len(ids) > 0 {
		return &ImageOfBuildError{BuildIDs: append([]int64{}, ids...)}
	}
	err = s.docker.RemoveImage(ctx, img.ID)
	switch {
	case errors.Is(err, docker.ErrInUse):
		return ErrInUse
	case errors.Is(err, docker.ErrNotFound), errors.Is(err, docker.ErrNotManaged):
		return ErrNotFound
	}
	return err
}

// Volumes returns the managed volumes. ServerID is null when the labelled
// server no longer exists; Current is true for a server's current data
// volume.
func (s *Service) Volumes(ctx context.Context) ([]apitypes.Volume, error) {
	vols, err := s.docker.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	known := map[int64]bool{}
	current := map[string]int64{}
	for _, r := range rows {
		known[r.ID] = true
		if r.DataVolume != "" {
			current[r.DataVolume] = r.ID
		}
	}
	out := make([]apitypes.Volume, 0, len(vols))
	for _, v := range vols {
		av := apitypes.Volume{Name: v.Name, ServerName: v.ServerName, CreatedAt: v.Created, InUse: v.InUse}
		if v.ServerID != 0 && known[v.ServerID] {
			id := v.ServerID
			av.ServerID = &id
			av.Current = current[v.Name] == id
		}
		out = append(out, av)
	}
	return out, nil
}

// RemoveVolume deletes a managed volume that no container mounts and that
// is not a server's current data volume (ErrVolumeCurrent, also for a
// volume a running job created and has not recorded yet). Unknown and
// unmanaged volumes are both ErrNotFound.
func (s *Service) RemoveVolume(ctx context.Context, name string) error {
	if err := docker.ValidateVolumeName(name); err != nil {
		return fieldErr("name", "invalid volume name")
	}
	if _, pending := s.pendingVolumes.Load(name); pending {
		return ErrVolumeCurrent
	}
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.DataVolume == name {
			return ErrVolumeCurrent
		}
	}
	err = s.docker.RemoveVolume(ctx, name)
	switch {
	case errors.Is(err, docker.ErrNotFound), errors.Is(err, docker.ErrNotManaged):
		return ErrNotFound
	case errors.Is(err, docker.ErrInUse):
		return ErrInUse
	}
	return err
}
