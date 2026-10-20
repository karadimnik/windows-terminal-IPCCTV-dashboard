//go:build windows

package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

const (
	importTimeout   = 15 * time.Second
	importMaxOutput = 16 << 10
	importPathEnv   = "HOLOCRON_IMPORT_PATH"
	createNoWindow  = 0x08000000
)

// The script reads the file with PowerShell's own Import-Clixml (DPAPI, this user only) and writes
// {"user":..,"password":..} as UTF-8 to stdout. The path arrives in the environment; the script
// text holds no secret and the command line holds none either.
const importScript = `$ErrorActionPreference = 'Stop'
try {
  $c = Import-Clixml -LiteralPath $env:` + importPathEnv + `
  if ($c -isnot [System.Management.Automation.PSCredential]) { throw 'the file does not hold a PSCredential' }
  $j = [pscustomobject]@{ user = $c.UserName; password = $c.GetNetworkCredential().Password } | ConvertTo-Json -Compress
  $b = [System.Text.Encoding]::UTF8.GetBytes($j)
  $o = [Console]::OpenStandardOutput(); $o.Write($b, 0, $b.Length); $o.Flush()
} catch {
  [Console]::Error.Write($_.Exception.Message)
  exit 3
}`

func encodeCommand(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 0, 2*len(u))
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// capped keeps at most max bytes and fails the write past that.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		return 0, errors.New("output too large")
	}
	return c.buf.Write(p)
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ImportLegacyCameraCreds stores the login of the legacy camera_creds.xml as camera.user and
// camera.password in dst. It never parses the file: a child powershell.exe (hidden, not elevated,
// 15 s limit) runs Import-Clixml and hands the login back on stdout.
func ImportLegacyCameraCreds(ctx context.Context, path string, dst Provider) error {
	if path == "" {
		return errors.New("secrets: import: no path given")
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return fmt.Errorf("secrets: import: %s is not a readable file", path)
	}
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()

	ps := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodeCommand(importScript))
	cmd.Env = append(os.Environ(), importPathEnv+"="+path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, errOut := &capped{max: importMaxOutput}, &capped{max: 2048}
	cmd.Stdout, cmd.Stderr = out, errOut

	runErr := cmd.Run()
	raw := out.buf.Bytes()
	defer wipe(raw)
	if ctx.Err() != nil {
		return fmt.Errorf("secrets: import: powershell did not finish within %s", importTimeout)
	}
	if runErr != nil {
		// stderr carries only an exception message, which never holds the login
		return fmt.Errorf("secrets: import: powershell failed: %v: %s", runErr, strings.TrimSpace(errOut.buf.String()))
	}
	var login struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(raw, &login); err != nil {
		return errors.New("secrets: import: the helper answered with something unreadable")
	}
	if login.User == "" || login.Password == "" {
		return errors.New("secrets: import: the file holds an empty user or password")
	}
	if err := checkValue(login.User); err != nil {
		return fmt.Errorf("secrets: import: user: %w", err)
	}
	if err := checkValue(login.Password); err != nil {
		return fmt.Errorf("secrets: import: password: %w", err)
	}
	if err := dst.Put(CameraUserName, login.User); err != nil {
		return fmt.Errorf("secrets: import: store %s: %w", CameraUserName, err)
	}
	if err := dst.Put(CameraPasswordName, login.Password); err != nil {
		return fmt.Errorf("secrets: import: store %s: %w", CameraPasswordName, err)
	}
	return nil
}
