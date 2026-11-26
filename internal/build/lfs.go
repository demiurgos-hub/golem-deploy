package build

import "errors"

// LFSInclude returns the lfs.include patterns of the golem-deploy.yaml at
// the root of the checkout dir, or nil when the file or the key is absent
// (download everything). The file is read through repoFS (contained to the
// checkout, no symbolic link, regular file, size-capped) and parsed with
// ParseLFSInclude: only the lfs section is validated here, the rest when a
// build starts. Every error is a *UserError whose message names the file
// and is safe to show to users.
//
// The daemon passes it to internal/git as git.Options.LFSInclude: the git
// runner calls it after checking out a commit and before "git lfs pull".
func LFSInclude(dir string) ([]string, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, &UserError{Msg: ConfigFile + " could not be read", Err: err}
	}
	defer repo.Close()
	raw, err := repo.readFile(ConfigFile, maxConfigBytes)
	if errors.Is(err, errMissing) {
		return nil, nil
	}
	if err != nil {
		var ue *UserError
		if errors.As(err, &ue) {
			return nil, err
		}
		return nil, &UserError{Msg: ConfigFile + " could not be read", Err: err}
	}
	return ParseLFSInclude(raw)
}
