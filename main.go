package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/LeonTing1010/mole/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		if errors.Is(err, cmd.ErrDeployInterrupted) {
			os.Exit(130)
		}
		os.Exit(1)
	}
}
