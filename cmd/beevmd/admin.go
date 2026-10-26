package main

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/om8P/BeeVM/internal/registry"
)

// The tenant and key commands work directly on the database file, so they need no admin
// password on the network: whoever can run beevmd on the server is the operator.

func runTenant(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: beevmd tenant add <name> | beevmd tenant ls")
		return 2
	}
	reg, err := openRegistry(beevmHome())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer reg.Close()

	switch args[0] {
	case "add":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: beevmd tenant add <name>")
			return 2
		}
		t, err := reg.CreateTenant(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		key, _, err := reg.CreateKey(t.ID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Printf("tenant %q created.\n\n", t.Name)
		printNewKey(key)
		return 0
	case "ls":
		tenants, err := reg.ListTenants()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TENANT\tCREATED")
		for _, t := range tenants {
			fmt.Fprintf(tw, "%s\t%s\n", t.Name, t.CreatedAt.Format(time.DateTime))
		}
		tw.Flush()
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown tenant command %q\n", args[0])
	return 2
}

func runKey(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: beevmd key add <tenant> | key ls [tenant] | key revoke <prefix>")
		return 2
	}
	reg, err := openRegistry(beevmHome())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer reg.Close()

	switch args[0] {
	case "add":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: beevmd key add <tenant>")
			return 2
		}
		t, err := reg.GetTenant(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		key, _, err := reg.CreateKey(t.ID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		printNewKey(key)
		return 0
	case "ls":
		tenant := ""
		if len(args) > 1 {
			tenant = args[1]
		}
		keys, err := reg.ListKeys(tenant)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TENANT\tKEY PREFIX\tCREATED\tSTATUS")
		for _, k := range keys {
			state := "active"
			if k.Revoked {
				state = "revoked"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k.Tenant, k.Prefix, k.CreatedAt.Format(time.DateTime), state)
		}
		tw.Flush()
		return 0
	case "revoke":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: beevmd key revoke <prefix>   (see: beevmd key ls)")
			return 2
		}
		if err := reg.RevokeKey(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			if errors.Is(err, registry.ErrAmbiguous) {
				fmt.Fprintln(os.Stderr, "give a longer prefix")
			}
			return 1
		}
		fmt.Println("key revoked")
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown key command %q\n", args[0])
	return 2
}

func printNewKey(key string) {
	fmt.Println("API key (shown only once, save it now):")
	fmt.Println()
	fmt.Println("  " + key)
	fmt.Println()
	fmt.Println("Use it with:  export BEEVM_API_KEY=" + key)
}
