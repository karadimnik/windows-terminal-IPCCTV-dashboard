package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"holocron/internal/cli"
	"holocron/internal/web"
)

// maxPasswordBytes bounds the line read; the 12-128 character policy is the web service's.
const maxPasswordBytes = 1024

// userAdmin opens the auth folder: --auth-dir, else web.authDir, else the web app's default folder.
func userAdmin(ctx context.Context, e *env, dir string) *web.UserAdmin {
	if dir == "" {
		dir = e.cfg.Web.AuthDir
	}
	return web.Users(dir, e.lg.For("auth"))
}

func userFlags(e *env, name string) (*flag.FlagSet, *string) {
	fs := newFlags(e, name)
	dir := fs.String("auth-dir", "", "folder of users.json (default: web.authDir)")
	return fs, dir
}

// userError maps the facade's refusals to exit codes: what the caller supplied is usage (2), a
// taken or missing name a refusal (1), anything else a failure.
func userError(err error) error {
	switch {
	case err == nil:
		return nil
	case web.IsInputError(err):
		return usageErr("%v; nothing changed", err)
	case errors.Is(err, web.ErrUserExists):
		return cli.Refuse("%v; use holocron user passwd to change the password", err)
	case errors.Is(err, web.ErrNoSuchUser):
		return cli.Refuse("%v", err)
	}
	return failed("%v", err)
}

func cmdUserSet(ctx context.Context, e *env, verb string) error {
	fs, dir := userFlags(e, "user "+verb)
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("user "+verb, pos, 1, 1); err != nil {
		return err
	}
	name := pos[0]
	password, err := e.secretInput("Password for "+name+" (12 to 128 characters; not shown): ", maxPasswordBytes)
	if err != nil {
		return err
	}
	e.lg.Redactor().Register(password)
	admin := userAdmin(ctx, e, *dir)
	actor := cli.ActorFrom(ctx)
	if verb == "add" {
		err = admin.Add(actor, name, password)
	} else {
		err = admin.SetPassword(actor, name, password)
	}
	if err != nil {
		return userError(err)
	}
	if verb == "add" {
		fmt.Fprintf(e.out, "created user %s (role admin) in %s\n", name, admin.Dir())
	} else {
		fmt.Fprintf(e.out, "changed the password of %s; every session of that user was ended\n", name)
	}
	return nil
}

func cmdUserRemove(ctx context.Context, e *env) error {
	fs, dir := userFlags(e, "user remove")
	force := fs.Bool("force", false, "remove the last user even though a TLS listener is configured")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("user remove", pos, 1, 1); err != nil {
		return err
	}
	admin := userAdmin(ctx, e, *dir)
	users, err := admin.List()
	if err != nil {
		return failed("%v", err)
	}
	if e.cfg.Web.TLSListen != "" && !*force && len(users) == 1 && strings.EqualFold(users[0].Username, pos[0]) {
		return cli.Refuse("%s is the last user and web.tlsListen (%s) is configured: the web app would refuse to start without a user; add another first or pass --force", users[0].Username, e.cfg.Web.TLSListen)
	}
	if err := admin.Remove(cli.ActorFrom(ctx), pos[0]); err != nil {
		return userError(err)
	}
	fmt.Fprintf(e.out, "removed user %s and ended its sessions\n", pos[0])
	return nil
}

func cmdUserList(ctx context.Context, e *env) error {
	fs, dir := userFlags(e, "user list")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("user list", pos, 0, 0); err != nil {
		return err
	}
	users, err := userAdmin(ctx, e, *dir).List()
	if err != nil {
		return failed("%v", err)
	}
	if e.json {
		return printJSON(e.out, users)
	}
	if len(users) == 0 {
		fmt.Fprintln(e.out, "no users (add one with: holocron user add <name>)")
		return nil
	}
	fmt.Fprintf(e.out, "%-24s %-8s %s\n", "NAME", "ROLE", "CREATED")
	for _, u := range users {
		fmt.Fprintf(e.out, "%-24s %-8s %s\n", u.Username, u.Role, u.CreatedAt.Local().Format(time.RFC3339))
	}
	return nil
}

func cmdUserUnlock(ctx context.Context, e *env) error {
	fs, dir := userFlags(e, "user unlock")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("user unlock", pos, 1, 1); err != nil {
		return err
	}
	if err := userAdmin(ctx, e, *dir).Unlock(cli.ActorFrom(ctx), pos[0]); err != nil {
		return userError(err)
	}
	fmt.Fprintf(e.out, "unlock requested for %q: a running web app forgets its failed sign-ins for that name at the next attempt (restarting it clears all of them)\n", pos[0])
	return nil
}

// ---------------------------------------------------------------------------
// tls
// ---------------------------------------------------------------------------

func tlsFlags(e *env, name string) (*flag.FlagSet, *string) {
	fs := newFlags(e, name)
	dir := fs.String("tls-dir", "", "folder of the CA and server certificate (default: web.tlsDir)")
	return fs, dir
}

func tlsDir(e *env, flagDir string) string {
	if flagDir != "" {
		return flagDir
	}
	return e.cfg.Web.TLSDir
}

func cmdTLSCA(ctx context.Context, e *env) error {
	fs, dir := tlsFlags(e, "tls ca")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("tls ca", pos, 0, 0); err != nil {
		return err
	}
	info, err := web.CAFor(tlsDir(e, *dir))
	if err != nil {
		return failed("cannot prepare the local CA: %v", err)
	}
	if e.json {
		return printJSON(e.out, map[string]any{"file": info.Path, "name": info.Subject, "notAfter": info.NotAfter, "sha256": info.Fingerprint})
	}
	web.WriteCAInstructions(e.out, info)
	return nil
}

func cmdTLSPrint(ctx context.Context, e *env) error {
	fs, dir := tlsFlags(e, "tls print")
	out := fs.String("out", "", "write the certificate to this file instead of standard output")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("tls print", pos, 0, 0); err != nil {
		return err
	}
	pem, err := web.CACertificatePEM(tlsDir(e, *dir))
	if errors.Is(err, os.ErrNotExist) {
		return failed("there is no local CA yet: run holocron tls ca (or start the web app with web.tlsListen) first")
	}
	if err != nil {
		return failed("%v", err)
	}
	if *out == "" {
		_, err = e.out.Write(pem)
		return err
	}
	if err := os.WriteFile(*out, pem, 0o644); err != nil {
		return failed("cannot write %s: %v", *out, err)
	}
	fmt.Fprintf(e.out, "wrote the CA certificate (no key) to %s\n", *out)
	return nil
}

func cmdTLSRotate(ctx context.Context, e *env) error {
	fs, dir := tlsFlags(e, "tls rotate")
	yes := fs.Bool("yes", false, "confirm: delete the CA and server certificate")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("tls rotate", pos, 0, 0); err != nil {
		return err
	}
	if !*yes {
		return usageErr("holocron tls rotate deletes the CA and server certificate, so EVERY device must install the new CA afterwards; run it again with --yes to confirm")
	}
	removed, left, err := web.RotateCA(tlsDir(e, *dir))
	if err != nil {
		return failed("%v", err)
	}
	for _, p := range removed {
		fmt.Fprintf(e.out, "deleted %s\n", p)
	}
	for _, p := range left {
		fmt.Fprintf(e.out, "left alone (not a certificate file): %s\n", p)
	}
	if len(removed) == 0 {
		fmt.Fprintln(e.out, "there was no CA to delete; a new one is created when the web app starts with web.tlsListen (or by holocron tls ca)")
		return nil
	}
	fmt.Fprintln(e.out, "the old CA is gone: a fresh CA and server certificate are created when the web app next starts (holocron restart, or holocron tls ca now); EVERY device must install the new CA (holocron tls ca shows the steps and the new fingerprint)")
	return nil
}
