// Command beevm is the user-facing CLI: deploy a function, invoke it, list and remove functions.
//
// It talks to a running beevmd over HTTP. Settings come from flags or the environment:
//
//	BEEVM_URL      server address (default http://127.0.0.1:8080)
//	BEEVM_API_KEY  your API key (the operator creates it with: beevmd tenant add <name>)
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/om8P/BeeVM/internal/apiclient"
)

type settings struct {
	url string
	key string
}

func main() {
	// Ctrl-C cancels the running request.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRoot().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}

// exitError lets a command choose the process exit code.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func newRoot() *cobra.Command {
	var s settings
	root := &cobra.Command{
		Use:           "beevm",
		Short:         "Deploy and run functions on BeeVM",
		SilenceUsage:  true, // do not print the whole help on every error
		SilenceErrors: true, // main prints errors itself
	}
	root.PersistentFlags().StringVar(&s.url, "url", "", "server address (default $BEEVM_URL or http://127.0.0.1:8080)")
	root.PersistentFlags().StringVar(&s.key, "key", "", "API key (default $BEEVM_API_KEY)")
	root.AddCommand(newDeploy(&s), newInvoke(&s), newLs(&s), newRm(&s))
	return root
}

// client builds the API client from flags and environment.
func (s *settings) client() (*apiclient.Client, error) {
	url := firstNonEmpty(s.url, os.Getenv("BEEVM_URL"), "http://127.0.0.1:8080")
	key := firstNonEmpty(s.key, os.Getenv("BEEVM_API_KEY"))
	if key == "" {
		return nil, &exitError{2, "no API key: set BEEVM_API_KEY or use --key (the operator creates one with: beevmd tenant add <name>)"}
	}
	return apiclient.New(url, key), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
