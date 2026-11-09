package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func TestReadOnlyCLIListMissingStoreIsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	for _, args := range [][]string{
		{"-p", "readonly", "list", "--json"},
		{"-p", "readonly", "group", "list", "--json"},
	} {
		cmd := exec.Command(channelsCLIBinary(t), args...)
		output, err := cmd.CombinedOutput()
		if err != nil || (args[2] == "list" && strings.TrimSpace(string(output)) != "[]") {
			t.Fatalf("agent-deck %s on a fresh profile: err=%v output=%s", strings.Join(args, " "), err, output)
		}
	}
	path, err := session.GetDBPathForProfile("readonly")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("list --json created state.db: %v", err)
	}
}

func TestReadOnlyCLIDoesNotStampProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	path, err := session.GetDBPathForProfile("readonly")
	if err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.SaveInstances([]*statedb.InstanceRow{
		{ID: "parent", Title: "parent", Tool: "shell", Status: "stopped", CreatedAt: now, LastAccessed: now},
		{ID: "child", Title: "child", ParentSessionID: "parent", Tool: "shell", Status: "stopped", CreatedAt: now, LastAccessed: now},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"-p", "readonly", "list", "--json"},
		{"-p", "readonly", "group", "list", "--json"},
		{"-p", "readonly", "session", "children", "parent", "--json"},
		{"-p", "readonly", "session", "children", "parent", "--follow", "--until-done", "--interval", "1ms", "--heartbeat", "0"},
		{"-p", "readonly", "session", "show", "parent", "--json"},
	} {
		cmd := exec.Command(channelsCLIBinary(t), args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("agent-deck %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		check, err := statedb.OpenReadOnlyLive(path)
		if err != nil {
			t.Fatal(err)
		}
		stamp, err := check.GetMeta("profile")
		_ = check.Close()
		if err != nil {
			t.Fatal(err)
		}
		if stamp != "" {
			t.Fatalf("agent-deck %s stamped read-only state.db with %q", strings.Join(args, " "), stamp)
		}
	}
}
