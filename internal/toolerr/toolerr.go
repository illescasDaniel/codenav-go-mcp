// Package toolerr renders expected tool failures as agent-readable text.
package toolerr

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// InputError is a caller mistake (bad name, unsupported file, ...).
type InputError struct {
	Msg string
	// Hint lists names the caller probably meant (case-insensitive matches), when there are any.
	Hint []string
	// Trivial marks a mistake in the arguments themselves (a bad identifier, an old_string that is not in the
	// file): the Go configuration of the workspace has nothing to do with it.
	Trivial bool
}

func (e *InputError) Error() string { return e.Msg }

// Inputf builds an InputError.
func Inputf(format string, args ...any) error { return &InputError{Msg: fmt.Sprintf(format, args...)} }

// Argf builds an InputError for a mistake in the call's arguments, unrelated to the workspace.
func Argf(format string, args ...any) error {
	return &InputError{Msg: fmt.Sprintf(format, args...), Trivial: true}
}

// IsTrivial reports whether err is an argument mistake (see InputError.Trivial).
func IsTrivial(err error) bool {
	var ie *InputError
	return errors.As(err, &ie) && ie.Trivial
}

// Format turns err into the text a tool returns; ok is false for unexpected errors.
func Format(err error) (text string, ok bool) {
	var (
		re  *lsp.RequestError
		ex  *lsp.ExitedError
		ie  *InputError
		ip  *lsp.InvalidPositionError
		pe  *fs.PathError
		dle = context.DeadlineExceeded
	)
	switch {
	case errors.As(err, &re):
		if re.Code == -1 { // synthesised on server exit
			return re.Message, true
		}
		return fmt.Sprintf("LSP error on %s: %s", re.Method, re.Message), true
	case errors.As(err, &ex):
		return ex.Msg, true
	case errors.As(err, &ie):
		return ie.Msg, true
	case errors.As(err, &ip):
		return ip.Msg, true
	case errors.Is(err, dle):
		return "Language server timed out (it may still be loading packages); retry shortly.", true
	case errors.Is(err, os.ErrNotExist) && errors.As(err, &pe):
		return fmt.Sprintf("File not found: %s (relative paths resolve against the workspace root).", pe.Path), true
	case errors.As(err, &pe):
		return fmt.Sprintf("Cannot read %s: %v.", pe.Path, pe.Err), true
	}
	return "", false
}

// Text is Format that falls back to err.Error().
func Text(err error) string {
	if t, ok := Format(err); ok {
		return t
	}
	return err.Error()
}
