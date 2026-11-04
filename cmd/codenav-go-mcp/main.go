// Command codenav-go-mcp is an MCP server that gives AI agents type-resolved Go
// navigation and edits, backed by gopls. It speaks MCP over stdio.
//
// Install and register it with an MCP client:
//
//	go install github.com/illescasDaniel/codenav-go-mcp/cmd/codenav-go-mcp@latest
//	claude mcp add codenav-go -- codenav-go-mcp
//
// It needs gopls on PATH. Configuration is through environment variables:
//
//	CODENAV_GO_MCP_WORKSPACE    pin the project root
//	CODENAV_GO_MCP_READ_ONLY    1 refuses every write (previews still work)
//	CODENAV_GO_MCP_GOPLS        path to the gopls binary
//	CODENAV_GO_MCP_BUILD_FLAGS  extra build flags, e.g. -tags
//
// Flags: --version, --help. See https://github.com/illescasDaniel/codenav-go-mcp
// for the tool reference.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/illescasDaniel/codenav-go-mcp/internal/server"
)

var version = "1.0.1"

func main() {
	for _, a := range os.Args[1:] {
		switch a {
		case "--version", "-v":
			fmt.Println(server.ServerName, version)
			return
		case "--help", "-h":
			fmt.Printf("%s %s\nMCP server navigating Go code via gopls (stdio).\nEnv: %s, %s, %s, %s\n",
				server.ServerName, version, server.EnvWorkspace, server.EnvReadOnly, server.EnvGopls, server.EnvBuildFlags)
			return
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	s, m := server.New(version)
	defer m.Close()
	if err := s.Run(ctx, server.NewStdioTransport(os.Stdin, os.Stdout)); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
