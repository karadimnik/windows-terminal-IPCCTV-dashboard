package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// readLine reads one line of at most max bytes: the first line of a pipe or file, with a trailing
// CR/LF and a UTF-8 byte-order mark (what Windows PowerShell 5.1 can prepend) removed and nothing
// else touched. A longer line is an error, never a silent cut. ok is false when the input held no
// line at all.
func readLine(r io.Reader, max int) (line string, ok bool, err error) {
	br := bufio.NewReader(r)
	var buf []byte
	for {
		b, err := br.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, err
		}
		if b == '\n' {
			buf = append(buf, b)
			break
		}
		if len(buf) >= max {
			return "", false, fmt.Errorf("the line is longer than %d bytes", max)
		}
		buf = append(buf, b)
	}
	if len(buf) == 0 {
		return "", false, nil
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(buf), "\n"), "\r")
	return strings.TrimPrefix(s, "\ufeff"), true, nil
}

// secretInput reads a secret from standard input only (never from an argument or an environment
// variable): the first line of a pipe or file, or, when stdin is a console, one line typed
// without echo after the prompt on stderr.
func (e *env) secretInput(prompt string, max int) (string, error) {
	var (
		line string
		ok   bool
		err  error
	)
	if f, isFile := e.stdin.(*os.File); isFile && isTerminal(f) {
		fmt.Fprint(e.errw, prompt)
		err = withoutEcho(f, func() error {
			line, ok, err = readLine(f, max)
			return err
		})
		fmt.Fprintln(e.errw)
	} else {
		line, ok, err = readLine(e.stdin, max)
	}
	switch {
	case err != nil:
		return "", failed("cannot read standard input: %v", err)
	case !ok || line == "":
		return "", usageErr("no value on standard input; pipe it in (for example: printf '%%s\\n' \"$VALUE\" | holocron ...); nothing changed")
	}
	return line, nil
}
