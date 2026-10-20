package config

import (
	"fmt"
	"strings"
)

// Problem is one thing wrong in a document, with the key path where it is.
type Problem struct {
	Path    string // "SegmentSeconds", "ContainerProfiles.mkv.Extension", "cameras[2].Record"
	Message string
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.Message
	}
	return p.Path + ": " + p.Message
}

// ValidationError lists every problem found, not just the first.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.String()
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return fmt.Sprintf("%d problems: %s", len(parts), strings.Join(parts, "; "))
}

// Warning is something worth telling the Operator that does not stop the load.
type Warning struct {
	Path    string
	Message string
}

func (w Warning) String() string { return w.Path + ": " + w.Message }
