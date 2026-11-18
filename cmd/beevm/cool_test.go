package main

import (
	"strings"
	"testing"
)

func TestCoolArguments(t *testing.T) {
	url, key := testServer(t)
	flags := []string{"--url", url, "--key", key}
	if _, err := runCLI(t, "", append([]string{"cool"}, flags...)...); err == nil || !strings.Contains(err.Error(), "--all") {
		t.Errorf("no name: %v", err)
	}
	if _, err := runCLI(t, "", append([]string{"cool", "f", "--all"}, flags...)...); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("name and --all: %v", err)
	}
	if _, err := runCLI(t, "", append([]string{"cool", "--all", "-n", "2"}, flags...)...); err == nil || !strings.Contains(err.Error(), "not with --all") {
		t.Errorf("-n with --all: %v", err)
	}
	// The test server's runner has no warm pool: the server says so (501).
	if _, err := runCLI(t, "", append([]string{"cool", "--all"}, flags...)...); err == nil || !strings.Contains(err.Error(), "501") {
		t.Errorf("no warm pool: %v", err)
	}
}
