package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"holocron/internal/archive"
)

// Camera is one entry of cameras.json. Name, IP, Quality and Record are what the product reads;
// every other field in the file is kept untouched, in its place, across a Save (as Set-CamList does).
type Camera struct {
	Name    string
	IP      string
	Quality string // stream path; blank means stream1
	Record  bool   // only an explicit true records

	fields []field // as read, in file order
}

type field struct {
	key string
	raw json.RawMessage
}

// Cameras is the camera list plus the one fact about the file that must survive a rewrite.
type Cameras struct {
	List []Camera
	BOM  bool // the file started with a UTF-8 BOM
}

var knownCameraKeys = []string{"Name", "IP", "Quality", "Record"}

func canonicalCameraKey(k string) string {
	for _, c := range knownCameraKeys {
		if strings.EqualFold(k, c) {
			return c
		}
	}
	return ""
}

// LoadCameras reads cameras.json.
func LoadCameras(path string) (*Cameras, error) {
	c := &Cameras{}
	return c, c.Load(path)
}

// Load replaces the list with the content of path. A missing file is an error (Get-CamList throws).
// The error is a *ValidationError for a wrong type (a string where a boolean belongs); the list is
// then still filled, with the offending fields at their zero value, so a caller can report and go on.
func (c *Cameras) Load(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("camera list: %w", err)
	}
	return c.Parse(raw)
}

// Parse reads the document; see Load.
func (c *Cameras) Parse(data []byte) error {
	c.BOM = bytes.HasPrefix(data, []byte("\xef\xbb\xbf"))
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	c.List = nil
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return &ValidationError{Problems: []Problem{{Message: "the camera list is not a JSON array of cameras: " + errText(err)}}}
	}
	var probs []Problem
	c.List = make([]Camera, 0, len(items))
	for i, it := range items {
		p := func(key, format string, args ...any) {
			probs = append(probs, Problem{Path: fmt.Sprintf("cameras[%d].%s", i, key), Message: fmt.Sprintf(format, args...)})
		}
		if jsonKind(it) != "an object" {
			probs = append(probs, Problem{Path: fmt.Sprintf("cameras[%d]", i), Message: "must be an object, got " + jsonKind(it)})
			c.List = append(c.List, Camera{})
			continue
		}
		fields, err := orderedFields(it)
		if err != nil {
			probs = append(probs, Problem{Path: fmt.Sprintf("cameras[%d]", i), Message: err.Error()})
			c.List = append(c.List, Camera{})
			continue
		}
		cam := Camera{fields: fields}
		for _, f := range fields {
			key := canonicalCameraKey(f.key)
			if key == "" {
				continue
			}
			kind := jsonKind(f.raw)
			switch key {
			case "Record":
				if kind != "a boolean" {
					p(f.key, "must be true or false (a JSON boolean), got %s %s", kind, strings.TrimSpace(string(f.raw)))
					continue
				}
				_ = json.Unmarshal(f.raw, &cam.Record)
			default:
				if kind == "null" && key == "Quality" {
					continue
				}
				if kind != "a string" {
					p(f.key, "must be a string, got %s", kind)
					continue
				}
				var s string
				_ = json.Unmarshal(f.raw, &s)
				switch key {
				case "Name":
					cam.Name = s
				case "IP":
					cam.IP = s
				case "Quality":
					cam.Quality = s
				}
			}
		}
		c.List = append(c.List, cam)
	}
	if len(probs) > 0 {
		return &ValidationError{Problems: probs}
	}
	return nil
}

// orderedFields lists an object's members in document order.
func orderedFields(raw json.RawMessage) ([]field, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out []field
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, field{key: t.(string), raw: v})
	}
	return out, nil
}

func marshalNoEscape(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// encode writes the document: 4-space indent, a trailing newline.
func (c *Cameras) encode() ([]byte, error) {
	var arr bytes.Buffer
	arr.WriteByte('[')
	for i, cam := range c.List {
		if i > 0 {
			arr.WriteByte(',')
		}
		arr.WriteByte('{')
		fields := append([]field(nil), cam.fields...)
		have := map[string]bool{}
		for _, f := range fields {
			have[canonicalCameraKey(f.key)] = true
		}
		add := func(key string, present bool, raw json.RawMessage) {
			if !have[key] && present {
				fields = append(fields, field{key: key, raw: raw})
			}
		}
		add("Name", true, nil)
		add("IP", true, nil)
		add("Quality", cam.Quality != "", nil)
		add("Record", cam.Record, nil)
		first := true
		for _, f := range fields {
			raw := f.raw
			switch canonicalCameraKey(f.key) {
			case "Name":
				raw = marshalNoEscape(cam.Name)
			case "IP":
				raw = marshalNoEscape(cam.IP)
			case "Quality":
				raw = marshalNoEscape(cam.Quality)
			case "Record":
				raw = marshalNoEscape(cam.Record)
			}
			if !first {
				arr.WriteByte(',')
			}
			first = false
			arr.Write(marshalNoEscape(f.key))
			arr.WriteByte(':')
			arr.Write(raw)
		}
		arr.WriteByte('}')
	}
	arr.WriteByte(']')
	var out bytes.Buffer
	if err := json.Indent(&out, arr.Bytes(), "", "    "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// Save writes the list atomically: a temp file in the same folder, then a rename over the target,
// so a crash leaves the old file or the new one, never a truncated one. A UTF-8 BOM is written when
// the file on disk has one (checked at save time, like Set-CamList), else when this list was read
// from a file that had one and the file is gone. Fields the product does not know survive.
func (c *Cameras) Save(path string) error {
	body, err := c.encode()
	if err != nil {
		return err
	}
	bom := c.BOM
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		if f, err := os.Open(path); err == nil {
			head := make([]byte, 3)
			n, _ := f.Read(head)
			f.Close()
			bom = n == 3 && bytes.Equal(head, []byte("\xef\xbb\xbf"))
		}
	}
	if bom {
		body = append([]byte("\xef\xbb\xbf"), body...)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Chmod(name, mode); err != nil {
		return fail(err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// Find returns the index of the camera with that name (case-insensitive, as the web app does), or -1.
func (c *Cameras) Find(name string) int {
	for i, cam := range c.List {
		if strings.EqualFold(cam.Name, name) {
			return i
		}
	}
	return -1
}

// SafeName is the camera's folder name (archive.SafeName).
func (c Camera) SafeName() (string, error) { return archive.SafeName(c.Name) }

// Stream is the stream path, stream1 when blank.
func (c Camera) Stream() string {
	if strings.TrimSpace(c.Quality) == "" {
		return "stream1"
	}
	return strings.TrimSpace(c.Quality)
}

// Collision is a set of cameras that would share one archive folder.
type Collision struct {
	Folder  string   // the first camera's safe name
	Indexes []int    // positions in the list
	Names   []string // their names
}

// Collisions reports cameras whose names sanitise to the same folder, comparing case-insensitively
// (NTFS and macOS). PowerShell silently ignores the second camera of such a pair (IR V5); callers
// should surface this. Cameras with an unusable name are skipped (Validate reports them).
func (c *Cameras) Collisions() []Collision {
	groups := map[string]*Collision{}
	var order []string
	for i, cam := range c.List {
		safe, err := cam.SafeName()
		if err != nil {
			continue
		}
		k := archive.FolderKey(safe)
		g, ok := groups[k]
		if !ok {
			g = &Collision{Folder: safe}
			groups[k] = g
			order = append(order, k)
		}
		g.Indexes = append(g.Indexes, i)
		g.Names = append(g.Names, cam.Name)
	}
	var out []Collision
	for _, k := range order {
		if len(groups[k].Indexes) > 1 {
			out = append(out, *groups[k])
		}
	}
	return out
}

var (
	streamRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	hostLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

// Validate checks every camera and returns every problem at once: a name that cannot be a folder
// (archive.SafeName), a missing or malformed address, a malformed stream name, and folder
// collisions. The web layer's extra limits (48-character names, a required stream) stay with it.
func (c *Cameras) Validate() error {
	var probs []Problem
	add := func(i int, key, format string, args ...any) {
		probs = append(probs, Problem{Path: fmt.Sprintf("cameras[%d].%s", i, key), Message: fmt.Sprintf(format, args...)})
	}
	for i, cam := range c.List {
		if _, err := cam.SafeName(); err != nil {
			add(i, "Name", "%v", err)
		}
		if msg := validateHost(cam.IP); msg != "" {
			add(i, "IP", "%s", msg)
		}
		if q := strings.TrimSpace(cam.Quality); q != "" && !streamRE.MatchString(q) {
			add(i, "Quality", "a stream name may use only letters, digits, dot, dash and underscore, got %q", cam.Quality)
		}
	}
	for _, col := range c.Collisions() {
		for _, i := range col.Indexes[1:] {
			add(i, "Name", "%q and cameras[%d] %q would share the folder %q on disk; the recorder ignores the second", c.List[i].Name, col.Indexes[0], c.List[col.Indexes[0]].Name, col.Folder)
		}
	}
	if len(probs) > 0 {
		return &ValidationError{Problems: probs}
	}
	return nil
}

// validateHost accepts an IP literal or a plausible hostname; not a reachability test.
func validateHost(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return "an address is required - the camera's IP or hostname"
	}
	if net.ParseIP(v) != nil {
		return ""
	}
	if strings.ContainsAny(v, `/\@: `) {
		return "an address is the host only - no scheme, port, path or credentials"
	}
	if len(v) > 253 {
		return "a hostname may be at most 253 characters"
	}
	for _, label := range strings.Split(strings.TrimSuffix(v, "."), ".") {
		if !hostLabel.MatchString(label) {
			return "not a valid IP address or hostname"
		}
	}
	return ""
}
