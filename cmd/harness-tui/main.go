package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/tui"
)

var (
	serverAddr = flag.String("server", "localhost:8539", "harness-server host:port")
	repoFlag   = flag.String("repo", ".", "default repo path for submit popup")
)

func main() {
	flag.Parse()
	repoAbs, err := filepath.Abs(*repoFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repo:", err)
		os.Exit(1)
	}
	app := tui.New(tui.Config{
		Server:      *serverAddr,
		DefaultRepo: repoAbs,
	})
	p := tea.NewProgram(app, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
