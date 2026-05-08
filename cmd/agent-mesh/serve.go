package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Path to config YAML")
	port := fs.Int("port", 0, "Port override (default from config or 9090)")
	fs.Parse(args)

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	m, err := initMesh(*configPath, *port, "", "")
	if err != nil {
		slog.Error("init failed", "error", err)
		os.Exit(1)
	}
	defer m.Close()

	fmt.Fprintf(os.Stderr, "agent-mesh %s (%s) built %s\n", version, commit, date)

	if err := m.ServeHTTP(); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
