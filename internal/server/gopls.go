package server

import "github.com/illescasDaniel/codenav-go-mcp/internal/goplscmd"

// GoplsCommand locates gopls the one way every caller does (see goplscmd.Command): the server and the workspace tool
// report the same binary.
func GoplsCommand() (cmd []string, warning string, err error) {
	return goplscmd.Command()
}
