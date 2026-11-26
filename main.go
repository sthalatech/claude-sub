// Command claude-sub exposes a Claude subscription (Claude Pro/Max) as an
// Anthropic Messages API endpoint that coding agents such as Shelley can use.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

const version = "0.1.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("claude-sub: ")

	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "register":
		err = runRegister(args)
	case "status":
		err = runStatus(args)
	case "models":
		err = runModels(args)
	case "logout":
		err = runLogout(args)
	case "version", "-v", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `claude-sub %s - Claude subscription <-> Anthropic Messages API

Usage:
  claude-sub serve     [flags]   run the proxy + web login page
  claude-sub register  [flags]   register the model with a local Shelley
  claude-sub models    [flags]   list models the subscription can call
  claude-sub status    [flags]   show login state
  claude-sub logout    [flags]   forget the stored tokens

Run 'claude-sub <command> -h' for command flags.
`, version)
}

func addCommonFlags(fs *flag.FlagSet) {
	fs.String("state", "", "state directory (default `%s`)")
}

func stateDir(fs *flag.FlagSet, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("CLAUDE_SUB_STATE"); v != "" {
		return v
	}
	if os.Geteuid() == 0 {
		return "/var/lib/claude-sub"
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v + "/claude-sub"
	}
	home, _ := os.UserHomeDir()
	return home + "/.local/state/claude-sub"
}
