package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newCool(s *settings) *cobra.Command {
	var all bool
	var count int
	cmd := &cobra.Command{
		Use:   "cool [name]",
		Short: "Throw away a function's warm VMs, so its next call is a cold start again",
		Long: "Warm VMs are kept after a call (for the server's --keep-alive) and make the next call fast.\n" +
			"cool removes them now: idle VMs at once, busy ones when their call ends. The next call is\n" +
			"then restored from a snapshot, as after a long pause. -n N cools only N of them (idle ones\n" +
			"first, least recently used first); --all cools every VM of every function of yours.",
		Args: func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return errors.New("give a function name or --all, not both")
			}
			if all && count > 0 {
				return errors.New("-n works for one function, not with --all")
			}
			if count < 0 {
				return errors.New("-n must be 1 or more")
			}
			if !all && len(args) != 1 {
				return errors.New("give a function name, or --all")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := s.client()
			if err != nil {
				return err
			}
			var n int
			what := "your functions"
			if all {
				n, err = c.CoolAll(cmd.Context())
			} else {
				what = args[0]
				n, err = c.Cool(cmd.Context(), args[0], count)
			}
			if err != nil {
				return err
			}
			if n == 0 {
				fmt.Printf("no warm VMs for %s; the next call is a cold start\n", what)
				return nil
			}
			if count > 0 {
				// Only some were asked for: others of the function may still be warm.
				fmt.Printf("cooled %d VM%s of %s (any others stay warm)\n", n, plural(n), what)
				return nil
			}
			fmt.Printf("cooled %d VM%s of %s; the next call is a cold start\n", n, plural(n), what)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "cool every function of yours")
	cmd.Flags().IntVarP(&count, "count", "n", 0, "cool only this many VMs, idle ones first (0 = all of the function's)")
	return cmd
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
