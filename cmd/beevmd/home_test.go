package main

import (
	"errors"
	"testing"
)

func TestHomeFrom(t *testing.T) {
	users := map[string]string{"om": "/home/om"}
	userHome := func(n string) (string, error) {
		if h, ok := users[n]; ok {
			return h, nil
		}
		return "", errors.New("no such user")
	}
	own := func() (string, error) { return "/root", nil }
	for _, c := range []struct {
		name string
		env  map[string]string
		euid int
		want string
	}{
		{"BEEVM_HOME wins", map[string]string{"BEEVM_HOME": "/data/bv", "SUDO_USER": "om"}, 0, "/data/bv"},
		{"sudo: the user's folder, not /root", map[string]string{"SUDO_USER": "om"}, 0, "/home/om/.beevm"},
		{"not root: SUDO_USER ignored", map[string]string{"SUDO_USER": "om"}, 1000, "/root/.beevm"},
		{"unknown sudo user", map[string]string{"SUDO_USER": "ghost"}, 0, "/root/.beevm"},
		{"plain", map[string]string{}, 1000, "/root/.beevm"},
	} {
		got := homeFrom(func(k string) string { return c.env[k] }, c.euid, userHome, own)
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
