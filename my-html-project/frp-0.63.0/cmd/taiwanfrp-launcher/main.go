package main

import (
	"fmt"
	"os"

	"github.com/fatedier/frp/pkg/launcher"
)

func main() {
	if err := launcher.Run(os.Args); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}
