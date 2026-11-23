package servers

import (
	"context"
	"errors"
	"slices"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
)

// Images returns the managed images with the servers that use them.
func (s *Service) Images(ctx context.Context) ([]apitypes.Image, error) {
	imgs, err := s.docker.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	cts, err := s.docker.ManagedContainers(ctx)
	if err != nil {
		return nil, err
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
		}
		for _, r := range rows {
			if r.Image == img.ID {
				v.ServerIDs = append(v.ServerIDs, r.ID)
			}
		}
		v.InUse = len(v.ServerIDs) > 0 || slices.ContainsFunc(cts, func(c docker.Container) bool { return c.ImageID == img.ID })
		out = append(out, v)
	}
	return out, nil
}

// RemoveImage deletes a managed image no server references and no
// container uses. Unknown and unmanaged images are both ErrNotFound so the
// API does not reveal unmanaged images.
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
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Image == img.ID {
			return ErrInUse
		}
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
// server no longer exists.
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
	for _, r := range rows {
		known[r.ID] = true
	}
	out := make([]apitypes.Volume, 0, len(vols))
	for _, v := range vols {
		av := apitypes.Volume{Name: v.Name, ServerName: v.ServerName, CreatedAt: v.Created, InUse: v.InUse}
		if v.ServerID != 0 && known[v.ServerID] {
			id := v.ServerID
			av.ServerID = &id
		}
		out = append(out, av)
	}
	return out, nil
}

// RemoveVolume deletes a managed volume that no container mounts. Unknown
// and unmanaged volumes are both ErrNotFound.
func (s *Service) RemoveVolume(ctx context.Context, name string) error {
	if err := docker.ValidateVolumeName(name); err != nil {
		return fieldErr("name", "invalid volume name")
	}
	err := s.docker.RemoveVolume(ctx, name)
	switch {
	case errors.Is(err, docker.ErrNotFound), errors.Is(err, docker.ErrNotManaged):
		return ErrNotFound
	case errors.Is(err, docker.ErrInUse):
		return ErrInUse
	}
	return err
}
