package archive

import (
	"fmt"
	"strings"
	"unicode"
)

// Error kinds of a NameError (the errorKind column of testdata/golden/names/safe-names.json).
const (
	KindNoLegalCharacters = "no-legal-characters"
	KindReservedDevice    = "reserved-device-name"
)

// NameError is why a camera name cannot be a folder name.
type NameError struct {
	Name string
	Kind string
	Stem string // the reserved device name, for KindReservedDevice
}

func (e *NameError) Error() string {
	if e.Kind == KindReservedDevice {
		return fmt.Sprintf("Camera name '%s' resolves to the reserved Windows device name '%s'. Rename the camera in cameras.json.", e.Name, e.Stem)
	}
	return fmt.Sprintf("Camera name '%s' contains no characters that are legal in a Windows filename.", e.Name)
}

var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// isSpace is the .NET whitespace class (char.IsWhiteSpace and the regex \s): the ASCII controls
// 9-13, U+0085 and every separator (category Z). U+200B (zero width space) is not whitespace.
func isSpace(r rune) bool {
	return (r >= 9 && r <= 13) || r == 0x85 || unicode.Is(unicode.Z, r)
}

// SafeName turns a camera name into the folder name and file prefix, reproducing Get-CamSafeName
// (cam.common.ps1) exactly so existing folders keep their names: trim, whitespace runs to "_",
// remove < > : " / \ | ? *, remove control characters, trim trailing dots and spaces; refuse an
// empty result and the Windows device names (also on Linux, so an archive stays portable). No
// lowercasing, no length limit, no change for a leading "_" or a "%" (see FormatSegmentPattern).
func SafeName(name string) (string, error) {
	s := strings.TrimFunc(name, isSpace)

	var b strings.Builder
	inSpace := false
	for _, r := range s {
		switch {
		case isSpace(r):
			if !inSpace {
				b.WriteByte('_')
			}
			inSpace = true
			continue
		case strings.ContainsRune(`<>:"/\|?*`, r), r < 0x20, r == 0x7f:
			// removed after the whitespace pass, so it separates two runs ("a \x01 b" gives "a__b")
			inSpace = false
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	safe := strings.TrimRight(b.String(), ". ")

	if safe == "" {
		return "", &NameError{Name: name, Kind: KindNoLegalCharacters}
	}
	stem := safe
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	if reservedNames[strings.ToUpper(stem)] {
		return "", &NameError{Name: name, Kind: KindReservedDevice, Stem: stem}
	}
	return safe, nil
}

// FolderKey is the key under which two safe names would share one folder on a case-insensitive
// filesystem (Windows, macOS default): names with the same key collide (IR V5).
func FolderKey(safe string) string { return strings.ToLower(strings.ToUpper(safe)) }
