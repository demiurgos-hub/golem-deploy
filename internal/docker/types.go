package docker

// DockerImage describes an image as listed by the docker CLI.
type DockerImage struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Size       string `json:"size"`
	Created    string `json:"created"`
}

// StaticAddon describes a static ZIP archive from the static store that is
// extracted into a Docker build context at a relative destination.
type StaticAddon struct {
	Filename  string `json:"filename"`
	ExtractTo string `json:"extract_to"`
}
