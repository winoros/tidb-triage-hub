package main

import (
	"flag"
	"io"
	"os"
	"os/exec"
	"testing"
)

func TestProjectBinding(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("TRIAGE_HUB_URL", "")
	if err := exec.Command("git", "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := bindWorkbench([]string{"https://workbench.example/"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("child", 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir("child")
	for _, tc := range []struct {
		env  string
		args []string
		want string
	}{
		{"", nil, "https://workbench.example"},
		{"https://env.example", nil, "https://env.example"},
		{"https://env.example", []string{"--db", "test.db"}, ""},
		{"", []string{"--remote", "https://other.example"}, "https://other.example"},
	} {
		t.Setenv("TRIAGE_HUB_URL", tc.env)
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("db", "", "")
		cfg := bindRemoteFlags(fs)
		if err := cfg.parse(fs, tc.args); err != nil || cfg.BaseURL != tc.want {
			t.Fatalf("args %v: target %q, error %v", tc.args, cfg.BaseURL, err)
		}
	}
}
