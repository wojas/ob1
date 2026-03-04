package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/wojas/ob1/internal/commands"
	"github.com/wojas/ob1/internal/logutil"
	"github.com/wojas/ob1/internal/obsidianapi"
	"github.com/wojas/ob1/internal/userstore"
)

func main() {
	os.Exit(run())
}

func run() int {
	store, err := userstore.NewDefault()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var debug bool
	var dryRun bool
	var noCache bool
	var apiBase string

	root := &cobra.Command{
		Use:           "ob1",
		Short:         "Alternative Obsidian headless client",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().BoolVarP(&debug, "debug", "v", false, "enable debug logging")
	root.PersistentFlags().BoolVarP(&dryRun, "dry-run", "n", false, "show what would change without making local changes")
	root.PersistentFlags().BoolVar(&noCache, "no-cache", false, "skip reading and writing the local remote snapshot cache")
	root.PersistentFlags().StringVar(&apiBase, "api-base", defaultAPIBase(), "Obsidian API base URL")

	runtime := commands.Runtime{
		Store:     store,
		NewLogger: newLogger,
		DryRun:    &dryRun,
	}

	root.AddCommand(commands.NewLoginCommand(runtime, &apiBase, &debug))
	root.AddCommand(commands.NewCatCommand(runtime, &debug, &noCache))
	root.AddCommand(commands.NewGetCommand(runtime, &debug, &noCache))
	root.AddCommand(commands.NewInfoCommand(runtime, &apiBase, &debug))
	root.AddCommand(commands.NewListCommand(runtime, &debug, &noCache))
	root.AddCommand(commands.NewPullCommand(runtime, &debug, &noCache))
	root.AddCommand(commands.NewPutCommand(runtime, &debug, &noCache))
	root.AddCommand(commands.NewLogoutCommand(runtime, &apiBase, &debug))
	root.AddCommand(commands.NewVaultCommand(runtime, &apiBase, &debug))

	if err := root.ExecuteContext(context.Background()); err != nil {
		newLogger(debug).Error(err.Error())
		return 1
	}

	return 0
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	return slog.New(logutil.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}, term.IsTerminal(int(os.Stderr.Fd()))))
}

func defaultAPIBase() string {
	if value := strings.TrimSpace(os.Getenv("OB1_API_BASE")); value != "" {
		return value
	}

	return obsidianapi.DefaultBaseURL
}
