package main

import (
	"context"
	"os"

	"github.com/schjan/picolet/pkg/cli"
)

func main() {
	if err := cli.Execute(context.Background(), os.Args); err != nil {
		os.Exit(1)
	}
}
