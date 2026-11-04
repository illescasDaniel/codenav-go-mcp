package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/illescasDaniel/codenav-go-mcp/internal/server"
)

var version = "0.1.0-dev"

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
