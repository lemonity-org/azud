package main

import (
	"os"

	"github.com/adriancarayol/azud/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
