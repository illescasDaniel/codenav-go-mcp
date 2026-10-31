// Package gosrc has go/ast based helpers: syntax checks, formatting, generated-file detection.
package gosrc

import (
	"go/format"
	"go/parser"
	"go/token"
	"regexp"

	"golang.org/x/tools/imports"

	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// CheckSyntax parses src; a syntax error is returned as an InputError.
func CheckSyntax(filename string, src string) error {
	if _, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.SkipObjectResolution); err != nil {
		return toolerr.Inputf("not valid Go: %v", err)
	}
	return nil
}

// Format runs goimports (which includes gofmt) over src. If goimports cannot resolve
// imports it falls back to plain gofmt.
func Format(filename, src string) (string, error) {
	if err := CheckSyntax(filename, src); err != nil {
		return "", err
	}
	out, err := imports.Process(filename, []byte(src), &imports.Options{Comments: true, TabIndent: true, TabWidth: 8, FormatOnly: true})
	if err != nil {
		f, ferr := format.Source([]byte(src))
		if ferr != nil {
			return "", toolerr.Inputf("cannot format: %v", ferr)
		}
		return string(f), nil
	}
	return string(out), nil
}

var generatedRE = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// IsGenerated: the file carries the standard generated-code header.
func IsGenerated(src string) bool {
	head := src
	if len(head) > 4096 {
		head = head[:4096]
	}
	return generatedRE.MatchString(head)
}

// GofmtClean: text (LF line endings) is what plain gofmt leaves unchanged.
func GofmtClean(text string) bool {
	f, err := format.Source([]byte(text))
	return err == nil && string(f) == text
}
