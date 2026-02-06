package main

import (
	"os"

	"github.com/slashoor/slashoor/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
