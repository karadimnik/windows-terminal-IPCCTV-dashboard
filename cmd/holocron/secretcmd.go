package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"holocron/internal/app"
	"holocron/internal/cli"
	"holocron/internal/secrets"
)

// maxSecretBytes is the store's value limit (4096 bytes of UTF-8).
const maxSecretBytes = 4096

func openSecrets(e *env) (secrets.Provider, error) {
	p, err := app.OpenSecrets(e.cfg, e.opts.Platform, e.lg.Redactor())
	if err != nil {
		return nil, failed("cannot open the secrets store (secrets.backend %q): %v", e.cfg.Secrets.Backend, err)
	}
	return p, nil
}

func secretName(name string) error {
	if !secrets.ValidName(name) {
		return usageErr("%q is not a secret name: use 1 to 64 characters from a-z 0-9 . _ -", name)
	}
	return nil
}

func cmdSecretSet(ctx context.Context, e *env) error {
	pos, err := parseFlags(newFlags(e, "secret set"), e.args)
	if err != nil {
		return err
	}
	if len(pos) == 2 {
		return usageErr("holocron secret set: a secret is read from standard input only, never from an argument")
	}
	if err := wantArgs("secret set", pos, 1, 1); err != nil {
		return err
	}
	if err := secretName(pos[0]); err != nil {
		return err
	}
	value, err := e.secretInput("Value for "+pos[0]+" (not shown): ", maxSecretBytes)
	if err != nil {
		return err
	}
	e.lg.Redactor().Register(value)
	p, err := openSecrets(e)
	if err != nil {
		return err
	}
	if err := p.Put(pos[0], value); err != nil {
		if errors.Is(err, secrets.ErrReadOnly) || errors.Is(err, secrets.ErrInvalidValue) {
			return cli.Refuse("%s: %v", pos[0], err)
		}
		return failed("%s: %v", pos[0], err)
	}
	fmt.Fprintf(e.out, "stored %s (%s store)\n", pos[0], p.Backend())
	return nil
}

func cmdSecretDelete(ctx context.Context, e *env) error {
	pos, err := parseFlags(newFlags(e, "secret delete"), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("secret delete", pos, 1, 1); err != nil {
		return err
	}
	if err := secretName(pos[0]); err != nil {
		return err
	}
	p, err := openSecrets(e)
	if err != nil {
		return err
	}
	if err := p.Delete(pos[0]); err != nil {
		if errors.Is(err, secrets.ErrNotFound) || errors.Is(err, secrets.ErrReadOnly) {
			return cli.Refuse("%s: %v", pos[0], err)
		}
		return failed("%s: %v", pos[0], err)
	}
	fmt.Fprintf(e.out, "deleted %s (%s store)\n", pos[0], p.Backend())
	return nil
}

func cmdSecretList(ctx context.Context, e *env) error {
	pos, err := parseFlags(newFlags(e, "secret list"), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("secret list", pos, 0, 0); err != nil {
		return err
	}
	p, err := openSecrets(e)
	if err != nil {
		return err
	}
	names, err := p.List()
	if err != nil {
		return failed("cannot list the secrets: %v", err)
	}
	if names == nil {
		names = []string{}
	}
	if e.json {
		return printJSON(e.out, map[string]any{"backend": p.Backend(), "names": names})
	}
	fmt.Fprintf(e.out, "%d secret(s) in the %s store (names only)\n", len(names), p.Backend())
	for _, n := range names {
		fmt.Fprintln(e.out, " ", n)
	}
	return nil
}

// cmdSecretImport moves the old camera_creds.xml login into the store. The file is read by
// PowerShell's own Import-Clixml in a short child (internal/secrets), never parsed here; only the
// two names are printed.
func cmdSecretImport(ctx context.Context, e *env) error {
	fs := newFlags(e, "secret import")
	from := fs.String("from", "", "the old camera_creds.xml (default: next to cameras.json)")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("secret import", pos, 0, 0); err != nil {
		return err
	}
	if e.opts.Platform.Name() != "windows" {
		return cli.Refuse("secret import reads the Windows camera_creds.xml (DPAPI) and works on Windows only; on this OS use holocron secret set camera.user and camera.password")
	}
	path := *from
	if path == "" {
		path = filepath.Join(filepath.Dir(e.paths.Cameras), "camera_creds.xml")
	}
	p, err := openSecrets(e)
	if err != nil {
		return err
	}
	if err := secrets.ImportLegacyCameraCreds(ctx, path, p); err != nil {
		if errors.Is(err, secrets.ErrUnsupported) || errors.Is(err, secrets.ErrReadOnly) {
			return cli.Refuse("%v", err)
		}
		return failed("%v", err)
	}
	fmt.Fprintf(e.out, "imported %s and %s\n", secrets.CameraUserName, secrets.CameraPasswordName)
	return nil
}
