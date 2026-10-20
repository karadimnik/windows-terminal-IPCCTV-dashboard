package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"holocron/internal/cli"
	"holocron/internal/config"
	"holocron/internal/ctlapi"
)

// LoadCameras reads cameras.json; a missing file is an empty list (the first `cam add` creates it).
func LoadCameras(path string) (*config.Cameras, error) {
	c, err := config.LoadCameras(path)
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return &config.Cameras{}, nil
	}
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		return c, cli.Refuse("%s cannot be edited safely: %v", path, err)
	}
	return c, err
}

// findCamera is the exact name first, then the one case-insensitive match (PowerShell -eq).
func findCamera(c *config.Cameras, name string) int {
	for i, cam := range c.List {
		if cam.Name == name {
			return i
		}
	}
	return c.Find(name)
}

func notFound(c *config.Cameras, name string) error {
	names := make([]string, 0, len(c.List))
	for _, cam := range c.List {
		names = append(names, cam.Name)
	}
	return fmt.Errorf("camera %q: %w (cameras: %s)", name, ctlapi.ErrNotFound, strings.Join(names, ", "))
}

// checkOne validates one camera (name usable as a folder, address, stream) and the folder
// collisions of the whole list. Other entries' own problems are not this edit's business.
func checkOne(c *config.Cameras, i int) error {
	one := &config.Cameras{List: []config.Camera{c.List[i]}}
	if err := one.Validate(); err != nil {
		return cli.Refuse("%s", strings.ReplaceAll(err.Error(), "cameras[0]", c.List[i].Name))
	}
	for _, col := range c.Collisions() {
		for _, j := range col.Indexes {
			if j == i {
				return cli.Refuse("%s would share the folder %q with %s", c.List[i].Name, col.Folder, strings.Join(col.Names, ", "))
			}
		}
	}
	return nil
}

// SetRecord is `cam enable|disable` and POST /v1/cameras/{name}/record: set Record and save
// atomically (BOM and unknown fields kept). "all" means every camera unless one is named "all".
// Enabling checks that the name can become a folder; a no-op writes nothing.
func SetRecord(path, name string, on bool) ([]ctlapi.Changed, error) {
	c, err := LoadCameras(path)
	if err != nil {
		return nil, err
	}
	var idx []int
	if i := findCamera(c, name); i >= 0 {
		idx = []int{i}
	} else if strings.EqualFold(name, "all") {
		for i := range c.List {
			idx = append(idx, i)
		}
	} else {
		return nil, notFound(c, name)
	}
	var out []ctlapi.Changed
	dirty := false
	for _, i := range idx {
		cam := &c.List[i]
		if on {
			if _, err := cam.SafeName(); err != nil {
				return nil, cli.Refuse("%s cannot record: %v", cam.Name, err)
			}
		}
		ch := ctlapi.Changed{Target: cam.Name, Enabled: on, Changed: cam.Record != on}
		cam.Record = on
		dirty = dirty || ch.Changed
		out = append(out, ch)
	}
	if dirty {
		if err := c.Save(path); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AddCamera appends a camera (validated, unique name) and saves.
func AddCamera(path string, cam config.Camera) error {
	c, err := LoadCameras(path)
	if err != nil {
		return err
	}
	if findCamera(c, cam.Name) >= 0 {
		return cli.Refuse("a camera named %q exists already", cam.Name)
	}
	c.List = append(c.List, cam)
	if err := checkOne(c, len(c.List)-1); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return c.Save(path)
}

// RemoveCamera drops a camera and saves (its recordings stay on disk).
func RemoveCamera(path, name string) (config.Camera, error) {
	c, err := LoadCameras(path)
	if err != nil {
		return config.Camera{}, err
	}
	i := findCamera(c, name)
	if i < 0 {
		return config.Camera{}, notFound(c, name)
	}
	gone := c.List[i]
	c.List = append(c.List[:i:i], c.List[i+1:]...)
	return gone, c.Save(path)
}

// CameraChange is `cam set`: nil fields stay.
type CameraChange struct {
	Name, IP, Quality *string
	Record            *bool
}

// SetCamera edits one camera's fields, validates the result and saves.
func SetCamera(path, name string, ch CameraChange) (config.Camera, error) {
	c, err := LoadCameras(path)
	if err != nil {
		return config.Camera{}, err
	}
	i := findCamera(c, name)
	if i < 0 {
		return config.Camera{}, notFound(c, name)
	}
	cam := &c.List[i]
	if ch.Name != nil && *ch.Name != cam.Name {
		if j := findCamera(c, *ch.Name); j >= 0 && j != i {
			return config.Camera{}, cli.Refuse("a camera named %q exists already", *ch.Name)
		}
		cam.Name = *ch.Name
	}
	if ch.IP != nil {
		cam.IP = *ch.IP
	}
	if ch.Quality != nil {
		cam.Quality = *ch.Quality
	}
	if ch.Record != nil {
		cam.Record = *ch.Record
	}
	if err := checkOne(c, i); err != nil {
		return config.Camera{}, err
	}
	return *cam, c.Save(path)
}

// FindCamera loads the list and returns one camera.
func FindCamera(path, name string) (config.Camera, error) {
	c, err := LoadCameras(path)
	if err != nil {
		return config.Camera{}, err
	}
	i := findCamera(c, name)
	if i < 0 {
		return config.Camera{}, notFound(c, name)
	}
	return c.List[i], nil
}
