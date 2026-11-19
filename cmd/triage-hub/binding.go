package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/winoros/tidb-triage-hub/internal/httputil"
)

const bindingKey = "triagehub.workbench"

// Deployments outside Git retain standalone SQLite mode without requiring git.
func workbenchBinding() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		gitDir := filepath.Join(dir, ".git")
		info, err := os.Stat(gitDir)
		if err == nil && info.IsDir() {
			_, err = os.Stat(filepath.Join(gitDir, "HEAD"))
		}
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
	out, err := exec.Command("git", "config", "--local", "--get", bindingKey).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read workbench binding: %w", err)
	}
	u, err := httputil.ParseBaseURL(string(out))
	if err != nil {
		return "", fmt.Errorf("invalid project workbench binding")
	}
	return u.String(), nil
}

func bindWorkbench(args []string, out io.Writer) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: triage-hub bind [URL]")
	}
	if len(args) == 0 {
		target, err := workbenchBinding()
		if err != nil {
			return err
		}
		if target == "" {
			return fmt.Errorf("no project binding; run triage-hub bind URL in a Git checkout")
		}
		fmt.Fprintln(out, target)
		return nil
	}
	u, err := httputil.ParseBaseURL(args[0])
	if err != nil || strings.ContainsAny(args[0], "?#") {
		return fmt.Errorf("expected an HTTP(S) workbench base URL without credentials, query or fragment")
	}
	if msg, err := exec.Command("git", "config", "--local", bindingKey, u.String()).CombinedOutput(); err != nil {
		return fmt.Errorf("bind project workbench: %w: %s", err, strings.TrimSpace(string(msg)))
	}
	fmt.Fprintln(out, u.String())
	return nil
}

func (c *remoteConfig) parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !isFlagExplicit(fs, "remote") {
		if isFlagExplicit(fs, "db") {
			c.BaseURL = ""
			return nil
		}
		if !c.enabled() {
			target, err := workbenchBinding()
			if err != nil {
				return err
			}
			c.BaseURL = target
		}
	}
	if !c.enabled() || c.Token != "" || isFlagExplicit(fs, "token") {
		return nil
	}
	u, err := httputil.ParseBaseURL(c.BaseURL)
	if err != nil {
		return err
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(dir, "triage-hub", "credentials.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("workbench credentials must be a private regular file (mode 0600)")
	}
	var tokens map[string]string
	if err := json.NewDecoder(f).Decode(&tokens); err != nil {
		return fmt.Errorf("invalid workbench credentials JSON")
	}
	c.Token = tokens[u.String()]
	return nil
}
