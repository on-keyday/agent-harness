package main

import (
	"flag"
	"fmt"
	"os"
)

var (
	serverAddr = flag.String("server", "localhost:8539", "harness-server host:port")
	repoFlag   = flag.String("repo", ".", "default repo path for submit popup")
)

func main() {
	flag.Parse()
	fmt.Fprintf(os.Stderr, "harness-tui (skeleton): server=%s repo=%s\n", *serverAddr, *repoFlag)
}
