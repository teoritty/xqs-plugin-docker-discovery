package dockerapi

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

// pathSegment escapes an id for use as one segment of an Engine API path.
//
// Every id here came from the daemon and was pattern-checked on the way back in, and it is escaped
// anyway. The two checks answer different questions: the pattern rejects what is not an id, and
// this makes any id — including ones a future Docker mints in a shape nobody predicted — unable to
// end a segment early, start a query, or climb a directory. Safety that rests on a charset is
// safety that breaks the next time the charset is widened, which is exactly what happened when
// image ids turned out to contain a colon.
func pathSegment(id string) string { return url.PathEscape(id) }

// errorsAs is a tiny indirection so client.go can avoid importing errors twice over.
func errorsAs(err error, target any) bool { return errors.As(err, target) }

// Container is the subset of /containers/json this plugin draws.
//
// A projection rather than the whole payload: everything here appears on screen or decides a
// status tone, and a field nobody reads is a field that silently rots.
type Container struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	// Image is what the container was started with — usually a tag, which may since have moved.
	Image string `json:"Image"`
	// ImageID is the immutable id behind it, and the only thing an image row can be matched on.
	ImageID string `json:"ImageID"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Ports   []Port `json:"Ports"`
	Labels  map[string]string
	// Mounts and NetworkSettings are what makes a volume's and a network's dot mean something. The
	// daemon does not report "is anything using this volume" on either list endpoint, so it is
	// derived from the containers — exactly as image usage already is, and out of the same list, so
	// it costs no extra request.
	Mounts          []Mount         `json:"Mounts"`
	NetworkSettings NetworkSettings `json:"NetworkSettings"`
}

// Mount is one thing mounted into a container. Name is empty for a bind mount, which is the whole
// distinction that matters here: only a named volume can be pointed at from the volume list.
type Mount struct {
	Type string `json:"Type"`
	Name string `json:"Name"`
}

// NetworkSettings carries the networks a container is attached to, keyed by network NAME.
//
// The name, not the id, is what the map is keyed by — so matching a network row against it means
// matching on Network.Name. Two networks may not share a name, so this is unambiguous.
type NetworkSettings struct {
	Networks map[string]struct{} `json:"Networks"`
}

// Port is one published port mapping.
type Port struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

// Name returns the container's display name without the leading slash Docker prefixes.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return shortID(c.ID)
	}
	name := c.Names[0]
	if len(name) > 0 && name[0] == '/' {
		name = name[1:]
	}
	if name == "" {
		return shortID(c.ID)
	}
	return name
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ContainerDetail is the part of /containers/{id}/json the status tooltip needs.
type ContainerDetail struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		Restarting bool   `json:"Restarting"`
		Dead       bool   `json:"Dead"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
			Log    []struct {
				Output string `json:"Output"`
			} `json:"Log"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
}

// Image is the subset of /images/json this plugin draws.
type Image struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Size     int64    `json:"Size"`
	Created  int64    `json:"Created"`
}

// Volume is the subset of /volumes this plugin draws.
type Volume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
}

// Network is the subset of /networks this plugin draws.
type Network struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver"`
	Scope    string            `json:"Scope"`
	Internal bool              `json:"Internal"`
	Labels   map[string]string `json:"Labels"`
}

// ListContainers returns every container, running or not.
//
// `all` is always true: a tree that showed only running containers would answer "where did it go?"
// with silence, which is the question a user opens this for.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	var out []Container
	query := url.Values{"all": []string{"1"}}
	if err := c.GetJSON(ctx, "/containers/json", query, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// InspectContainer returns the detail behind a container's status dot.
func (c *Client) InspectContainer(ctx context.Context, id string) (ContainerDetail, error) {
	var detail ContainerDetail
	if err := c.GetJSON(ctx, "/containers/"+pathSegment(id)+"/json", nil, &detail); err != nil {
		return ContainerDetail{}, err
	}
	return detail, nil
}

// InspectRaw returns an object's inspect payload verbatim, for the detail dialog's code block.
func (c *Client) InspectRaw(ctx context.Context, kind, id string) ([]byte, error) {
	switch kind {
	case "container":
		return c.GetRaw(ctx, "/containers/"+pathSegment(id)+"/json", nil)
	case "image":
		return c.GetRaw(ctx, "/images/"+pathSegment(id)+"/json", nil)
	case "volume":
		return c.GetRaw(ctx, "/volumes/"+pathSegment(id), nil)
	case "network":
		return c.GetRaw(ctx, "/networks/"+pathSegment(id), nil)
	default:
		return nil, errors.New("dockerapi: unknown object kind")
	}
}

// ListImages returns every image, hiding intermediate layers.
func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	var out []Image
	if err := c.GetJSON(ctx, "/images/json", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListVolumes returns every volume.
func (c *Client) ListVolumes(ctx context.Context) ([]Volume, error) {
	var payload struct {
		Volumes []Volume `json:"Volumes"`
	}
	if err := c.GetJSON(ctx, "/volumes", nil, &payload); err != nil {
		return nil, err
	}
	return payload.Volumes, nil
}

// ListNetworks returns every network.
func (c *Client) ListNetworks(ctx context.Context) ([]Network, error) {
	var out []Network
	if err := c.GetJSON(ctx, "/networks", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- container lifecycle ---------------------------------------------------

// StartContainer starts a stopped container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.PostJSON(ctx, "/containers/"+pathSegment(id)+"/start", nil, nil, nil)
}

// StopContainer stops a running container, giving it the daemon's default grace period.
func (c *Client) StopContainer(ctx context.Context, id string) error {
	return c.PostJSON(ctx, "/containers/"+pathSegment(id)+"/stop", nil, nil, nil)
}

// RestartContainer stops and starts a container.
func (c *Client) RestartContainer(ctx context.Context, id string) error {
	return c.PostJSON(ctx, "/containers/"+pathSegment(id)+"/restart", nil, nil, nil)
}

// KillContainer sends SIGKILL.
func (c *Client) KillContainer(ctx context.Context, id string) error {
	return c.PostJSON(ctx, "/containers/"+pathSegment(id)+"/kill", nil, nil, nil)
}

// PauseContainer freezes a container's processes.
func (c *Client) PauseContainer(ctx context.Context, id string) error {
	return c.PostJSON(ctx, "/containers/"+pathSegment(id)+"/pause", nil, nil, nil)
}

// UnpauseContainer resumes a paused container.
func (c *Client) UnpauseContainer(ctx context.Context, id string) error {
	return c.PostJSON(ctx, "/containers/"+pathSegment(id)+"/unpause", nil, nil, nil)
}

// RemoveContainer deletes a container, optionally forcing it and taking its anonymous volumes.
func (c *Client) RemoveContainer(ctx context.Context, id string, force, removeVolumes bool) error {
	query := url.Values{}
	if force {
		query.Set("force", "1")
	}
	if removeVolumes {
		query.Set("v", "1")
	}
	return c.Delete(ctx, "/containers/"+pathSegment(id), query)
}

// RemoveImage deletes an image.
func (c *Client) RemoveImage(ctx context.Context, id string, force, noPrune bool) error {
	query := url.Values{}
	if force {
		query.Set("force", "1")
	}
	if noPrune {
		query.Set("noprune", "1")
	}
	return c.Delete(ctx, "/images/"+pathSegment(id), query)
}

// RemoveVolume deletes a volume.
func (c *Client) RemoveVolume(ctx context.Context, name string, force bool) error {
	query := url.Values{}
	if force {
		query.Set("force", "1")
	}
	return c.Delete(ctx, "/volumes/"+pathSegment(name), query)
}

// RemoveNetwork deletes a network.
func (c *Client) RemoveNetwork(ctx context.Context, id string) error {
	return c.Delete(ctx, "/networks/"+pathSegment(id), nil)
}

// CreateVolumeRequest is the body of a volume create.
type CreateVolumeRequest struct {
	Name       string            `json:"Name,omitempty"`
	Driver     string            `json:"Driver,omitempty"`
	DriverOpts map[string]string `json:"DriverOpts,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

// CreateVolume creates a volume.
func (c *Client) CreateVolume(ctx context.Context, req CreateVolumeRequest) error {
	return c.PostJSON(ctx, "/volumes/create", nil, req, nil)
}

// IPAMConfig is one subnet block of a network create.
type IPAMConfig struct {
	Subnet     string            `json:"Subnet,omitempty"`
	IPRange    string            `json:"IPRange,omitempty"`
	Gateway    string            `json:"Gateway,omitempty"`
	AuxAddress map[string]string `json:"AuxiliaryAddresses,omitempty"`
}

// CreateNetworkRequest is the body of a network create.
type CreateNetworkRequest struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver,omitempty"`
	Internal   bool              `json:"Internal,omitempty"`
	Attachable bool              `json:"Attachable,omitempty"`
	EnableIPv6 bool              `json:"EnableIPv6,omitempty"`
	Options    map[string]string `json:"Options,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	IPAM       *struct {
		Driver string       `json:"Driver,omitempty"`
		Config []IPAMConfig `json:"Config,omitempty"`
	} `json:"IPAM,omitempty"`
}

// CreateNetwork creates a network.
func (c *Client) CreateNetwork(ctx context.Context, req CreateNetworkRequest) error {
	return c.PostJSON(ctx, "/networks/create", nil, req, nil)
}

// --- streams ---------------------------------------------------------------

// LogOptions selects what a log stream carries.
type LogOptions struct {
	Follow     bool
	Timestamps bool
	Tail       int
}

// ContainerLogs opens a log stream. The caller owns the returned reader and the client's stream.
func (c *Client) ContainerLogs(ctx context.Context, id string, opts LogOptions) (readCloser, error) {
	query := url.Values{
		"stdout": []string{"1"},
		"stderr": []string{"1"},
	}
	if opts.Follow {
		query.Set("follow", "1")
	}
	if opts.Timestamps {
		query.Set("timestamps", "1")
	}
	if opts.Tail > 0 {
		query.Set("tail", strconv.Itoa(opts.Tail))
	} else {
		query.Set("tail", "all")
	}
	return c.Stream(ctx, "GET", "/containers/"+pathSegment(id)+"/logs", query)
}

// Events opens the daemon's event stream.
func (c *Client) Events(ctx context.Context) (readCloser, error) {
	return c.Stream(ctx, "GET", "/events", nil)
}

// readCloser is io.ReadCloser, aliased so this file does not import io for one name.
type readCloser interface {
	Read(p []byte) (int, error)
	Close() error
}
