// Command show prints one setting. It uses config.GetCfg to read the file.
package main

import (
	"fmt"
	"os"

	"fixture/config"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: show FILE KEY")
		os.Exit(2)
	}
	settings, err := config.GetCfg(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(settings[os.Args[2]])
}
