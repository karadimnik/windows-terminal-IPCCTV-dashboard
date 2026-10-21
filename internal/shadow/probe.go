package shadow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Stream is the part of one stream's layout the comparison looks at.
type Stream struct {
	Codec      string `json:"codec"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
	Channels   int    `json:"channels,omitempty"`
}

// Probe is what ffprobe says about one closed segment.
type Probe struct {
	Duration float64 // seconds, format=duration
	Video    *Stream // first video stream, nil when none
	Audio    *Stream // first audio stream, nil when none
	Videos   int
	Audios   int
}

// Prober reads the layout of a media file. The production one runs ffprobe; tests inject a fake.
type Prober interface {
	Probe(ctx context.Context, path string) (Probe, error)
}

// FFprobe runs ffprobe, hidden, once per call, with a timeout. The caller makes the calls one at a time.
type FFprobe struct {
	Path    string
	Timeout time.Duration // default 30 s
}

type probeJSON struct {
	Streams []struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func (f FFprobe) Probe(ctx context.Context, path string) (Probe, error) {
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.Path, "-v", "error", "-show_entries",
		"format=duration:stream=codec_type,codec_name,width,height,sample_rate,channels", "-of", "json", path)
	cmd.SysProcAttr = hiddenWindow()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Probe{}, fmt.Errorf("ffprobe: %w after %s", ctx.Err(), timeout)
		}
		return Probe{}, fmt.Errorf("ffprobe: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return ParseProbe(out.Bytes())
}

// ParseProbe reads ffprobe's JSON output (-of json) for the entries FFprobe asks for.
func ParseProbe(b []byte) (Probe, error) {
	var j probeJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return Probe{}, fmt.Errorf("ffprobe: unreadable output: %v", err)
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(j.Format.Duration), 64)
	if err != nil || d <= 0 {
		return Probe{}, fmt.Errorf("ffprobe: no duration (%q)", j.Format.Duration)
	}
	p := Probe{Duration: d}
	for _, s := range j.Streams {
		st := &Stream{Codec: s.CodecName, Width: s.Width, Height: s.Height, Channels: s.Channels}
		st.SampleRate, _ = strconv.Atoi(s.SampleRate)
		switch s.CodecType {
		case "video":
			p.Videos++
			if p.Video == nil {
				p.Video = st
			}
		case "audio":
			p.Audios++
			if p.Audio == nil {
				p.Audio = st
			}
		}
	}
	return p, nil
}
