package main

import (
	"os"

	"github.com/arrokh/tailge/internal/bootstrap"
	"github.com/arrokh/tailge/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, bootstrap.New()))
}
