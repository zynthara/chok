package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "init":
		if err := runInit(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "chok: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "chok: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`Usage:
  chok init [name]    Initialize a new chok project

  If name is provided, creates a directory with that name.
  If name is omitted, initializes in the current directory.`)
}
