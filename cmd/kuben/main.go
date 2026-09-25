// Command kuben is the Kuben server and its command line.
package main

import (
	"fmt"
	"os"

	"github.com/Teamtem-dev/kuben/internal/cli"
)

func main() {
	if err := cli.Root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(cli.ExitCode(err))
	}
}
