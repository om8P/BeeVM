package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/om8P/BeeVM/internal/apiclient"
)

// printJSON writes v as indented JSON: the --json output of the list commands, meant for
// scripts (jq) rather than people.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ---------------------------------------------------------------- get

func newGet(s *settings) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show one function's settings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := s.client()
			if err != nil {
				return err
			}
			f, err := c.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(os.Stdout, f)
			}
			handler := f.Handler
			if handler == "" {
				handler = "-"
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "name:\t%s\n", f.Name)
			fmt.Fprintf(tw, "runtime:\t%s\n", f.Runtime)
			fmt.Fprintf(tw, "handler:\t%s\n", handler)
			fmt.Fprintf(tw, "code:\t%s, sha256 %s\n", humanSize(f.CodeSize), f.CodeSHA256)
			fmt.Fprintf(tw, "memory:\t%d MiB\n", f.MemoryMiB)
			fmt.Fprintf(tw, "vcpus:\t%d\n", f.VCPUs)
			fmt.Fprintf(tw, "timeout:\t%s\n", time.Duration(f.TimeoutMS)*time.Millisecond)
			fmt.Fprintf(tw, "created:\t%s\n", f.CreatedAt.Local().Format(time.DateTime))
			fmt.Fprintf(tw, "updated:\t%s\n", f.UpdatedAt.Local().Format(time.DateTime))
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// ---------------------------------------------------------------- login, logout, whoami

// readKey reads one line (the API key) from r. When stdin is a terminal it prints a prompt
// first; the key is shown while typing, so prefer pasting it or piping it in.
func readKey(r io.Reader, prompt bool) (string, error) {
	if prompt {
		fmt.Fprint(os.Stderr, "API key (from beevmd tenant add / key add): ")
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func stdinIsTerminal() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func newLogin(s *settings) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Check an API key and save it, so later commands need no BEEVM_API_KEY",
		Long: "Asks for the API key (or takes --key, or reads it from standard input), checks it with the\n" +
			"server, and saves the key and the server address (--url) in ~/.config/beevm/config.json\n" +
			"(readable only by you). BEEVM_API_KEY, BEEVM_URL and the flags still win over the saved values.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _, err := s.resolve()
			if err != nil {
				return err
			}
			key := s.key
			if key == "" {
				if key, err = readKey(os.Stdin, stdinIsTerminal()); err != nil {
					return err
				}
			}
			if key == "" {
				return &exitError{2, "no API key given"}
			}
			me, err := apiclient.New(url, key).Me(cmd.Context())
			if err != nil {
				return fmt.Errorf("the server did not accept the key: %w", err)
			}
			path, err := saveConfig(config{URL: url, Key: key})
			if err != nil {
				return err
			}
			fmt.Printf("logged in to %s as tenant %s (saved in %s)\n", url, me.Tenant, path)
			return nil
		},
	}
}

func newLogout() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the key saved by beevm login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := removeConfig()
			if err != nil {
				return err
			}
			fmt.Println("removed", path)
			if os.Getenv("BEEVM_API_KEY") != "" {
				fmt.Println("note: BEEVM_API_KEY is still set in this terminal")
			}
			return nil
		},
	}
}

// keySource says where the key in use comes from, for whoami.
func (s *settings) keySource() string {
	switch {
	case s.key != "":
		return "--key"
	case os.Getenv("BEEVM_API_KEY") != "":
		return "BEEVM_API_KEY"
	}
	p, _ := configPath()
	return p
}

func newWhoami(s *settings) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the tenant your API key belongs to, and the server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _, err := s.resolve()
			if err != nil {
				return err
			}
			c, err := s.client()
			if err != nil {
				return err
			}
			me, err := c.Me(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(os.Stdout, map[string]string{"tenant": me.Tenant, "url": url, "key_from": s.keySource()})
			}
			fmt.Printf("tenant %s on %s (key from %s)\n", me.Tenant, url, s.keySource())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
