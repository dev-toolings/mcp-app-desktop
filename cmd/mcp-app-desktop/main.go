// mcp-app-desktop is an MCP server (stdio) that lets a coding agent launch,
// observe, drive and debug a desktop application built with Electron or
// Tauri.
//
//	mcp-app-desktop                 serve MCP over stdio (default)
//	mcp-app-desktop install         register with Claude Code (and --codex) and install the skill
//	mcp-app-desktop run-flow FILE   run a Maestro-style flow against a launched app (CI)
//	mcp-app-desktop version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/dev-toolings/mcp-app-desktop/internal/desktop"
	"github.com/dev-toolings/mcp-app-desktop/internal/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "mcp"
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "mcp", "serve":
		err = serve(args)
	case "install":
		err = install(args)
	case "run-flow":
		err = runFlow(args)
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "mcp-app-desktop: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-app-desktop: %v\n", err)
		os.Exit(1)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func usage() {
	fmt.Fprint(os.Stderr, `mcp-app-desktop: drive and debug an Electron or Tauri app from an MCP agent

  mcp-app-desktop [mcp] [--artifacts DIR] [--max-sessions N] [--idle-timeout D] [--static-tools]
                                            serve MCP over stdio (defaults: 3 sessions,
                                            unused launched apps stopped after 30m;
                                            framework tools listed with their sessions)
  mcp-app-desktop install [--dry-run] [--no-codex] [--dest DIR | --bin PATH]
                                            copy the binary to ~/.local/bin, register it
                                            with Claude Code (user scope) and Codex, and
                                            install the skill; --dry-run writes nothing
  mcp-app-desktop install --uninstall [--dry-run]
                                            remove registrations, skills and the copy
  mcp-app-desktop run-flow FILE (--target PATH | --command CMD...) [--env K=V]...
                                            launch the app, run a flow, exit 1 on failure
  mcp-app-desktop version
`)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	artifacts := fs.String("artifacts", "", "directory for screenshots and flow outputs")
	maxSessions := fs.Int("max-sessions", desktop.DefaultMaxSessions, "open sessions allowed at once (each is a whole app process tree)")
	idle := fs.Duration("idle-timeout", desktop.DefaultIdleTimeout, "stop a launched app unused for this long (0 disables)")
	static := fs.Bool("static-tools", os.Getenv("MCP_APP_DESKTOP_STATIC_TOOLS") == "1", "list every tool from the start (hosts that ignore tools/list_changed); default: framework tools appear with a session of their framework")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *idle == 0 {
		*idle = -1
	}
	s, err := mcp.New(mcp.Options{ArtifactsRoot: *artifacts, Version: version, MaxSessions: *maxSessions, IdleTimeout: *idle, StaticTools: *static})
	if err != nil {
		return err
	}
	defer s.Close()
	// Stop every launched application when the host disconnects or on a
	// signal: an agent's forgotten session must not outlive the server.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := mcpsrv.NewStdioServer(s.Build())
	// mcp-go runs 5 tool calls at once by default: five calls waiting on
	// stuck windows left status, trace and every other session queued
	// behind them. Each call is bounded (pageCallCeiling, actionWait), so
	// more workers cost nothing at rest.
	mcpsrv.WithWorkerPoolSize(32)(srv)
	srv.SetErrorLogger(log.New(os.Stderr, "[mcp-app-desktop] ", log.LstdFlags))
	fmt.Fprintln(os.Stderr, "[mcp-app-desktop] ready on stdio")
	if err := srv.Listen(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
