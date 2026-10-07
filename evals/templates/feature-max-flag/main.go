// Command numbered prints the lines of standard input with line numbers.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("numbered", flag.ContinueOnError)
	flags.SetOutput(stderr)
	start := flags.Int("start", 1, "number of the first line")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	scanner := bufio.NewScanner(stdin)
	for number := *start; scanner.Scan(); number++ {
		fmt.Fprintf(stdout, "%d\t%s\n", number, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(stderr, "numbered: %v\n", err)
		return 1
	}
	return 0
}
