package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTempCodexHome points both CODEX_HOME and HOME at a fresh temp dir so an
// installer test can never reach the developer's live config. A shell that
// exports CODEX_HOME made the HOME-only scoping of earlier tests target the
// real ~/.config/codex/config.toml and replace it with the bare marker block.
func useTempCodexHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	return getCodexConfigPath()
}

// largeCodexConfig mimics a real, dotfiles-managed config.toml: top-level
// keys, tables, comments, nested arrays and many project trust entries.
// notifyLines is inserted after the top-level keys; nil means no notify at all.
func largeCodexConfig(notifyLines []string) string {
	var b strings.Builder
	b.WriteString("check_for_update_on_startup = false\n")
	b.WriteString("approval_policy = \"on-request\"\n")
	b.WriteString("model = \"gpt-6-sol\"\n")
	b.WriteString("model_reasoning_effort = \"medium\"\n")
	b.WriteString("sandbox_mode = \"workspace-write\"\n")
	for _, line := range notifyLines {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n[agents]\n  default_subagent_model = \"gpt-5.6-terra\"\n")
	b.WriteString("\n# marketplaces\n[marketplaces.official]\n  url = \"https://example.invalid/marketplace\"\n")
	for i := 0; i < 20; i++ {
		b.WriteString("\n[mcp_servers.server" + strings.Repeat("x", i%3) + string(rune('a'+i)) + "]\n")
		b.WriteString("  command = \"npx\"\n  args = [\"-y\", \"@example/mcp@latest\", \"--flag\"]\n")
		b.WriteString("  [mcp_servers.server" + strings.Repeat("x", i%3) + string(rune('a'+i)) + ".env]\n    TOKEN = \"op://Automation/item/field\"\n")
	}
	for i := 0; i < 60; i++ {
		b.WriteString("\n[projects.\"/home/user/projects/repo" + string(rune('a'+i%26)) + strings.Repeat("z", i/26) + "\"]\n  trust_level = \"trusted\"\n")
	}
	b.WriteString("\n[hooks]\n  enabled = true\n")
	return b.String()
}

func writeCodexConfigForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCodexConfigForTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCodexHooksInstall_ReplacesBlockInPlaceAndPreservesLargeConfig(t *testing.T) {
	configPath := useTempCodexHome(t)
	legacyBlock := []string{codexNotifyMarkerBegin, "[notify]", `program = ["agent-deck", "codex-notify"]`, codexNotifyMarkerEnd}
	before := largeCodexConfig(legacyBlock)
	if n := strings.Count(before, "\n"); n < 250 {
		t.Fatalf("fixture too small to be representative: %d lines", n)
	}
	writeCodexConfigForTest(t, configPath, before)

	outcome, err := installCodexNotifyHook(configPath)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if outcome != codexInstallUpgraded {
		t.Fatalf("outcome = %v, want upgraded", outcome)
	}
	want := largeCodexConfig([]string{codexNotifyMarkerBegin, codexNotifyLine, codexNotifyMarkerEnd})
	got := readCodexConfigForTest(t, configPath)
	if got != want {
		t.Fatalf("config not preserved byte for byte\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Count(got, "notify = ") != 1 {
		t.Fatalf("expected exactly one notify key, got %d", strings.Count(got, "notify = "))
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600 preserved", info.Mode().Perm())
	}
}

func TestCodexHooksInstall_FreshInstallKeepsEveryExistingLine(t *testing.T) {
	configPath := useTempCodexHome(t)
	before := largeCodexConfig(nil)
	writeCodexConfigForTest(t, configPath, before)

	outcome, err := installCodexNotifyHook(configPath)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if outcome != codexInstallInstalled {
		t.Fatalf("outcome = %v, want installed", outcome)
	}
	got := readCodexConfigForTest(t, configPath)
	if !strings.HasPrefix(got, codexNotifyBlockText()+"\n") {
		t.Fatalf("block must lead the file so the top-level key precedes tables:\n%s", got[:200])
	}
	if !strings.HasSuffix(got, before) {
		t.Fatalf("existing content was altered")
	}
}

func TestCodexHooksInstall_ExistingTopLevelNotifyIsNotDuplicated(t *testing.T) {
	configPath := useTempCodexHome(t)
	// The dotfiles source keeps the exact notify line outside any markers.
	before := largeCodexConfig([]string{codexNotifyLine})
	writeCodexConfigForTest(t, configPath, before)
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := installCodexNotifyHook(configPath)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if outcome != codexInstallUnchanged {
		t.Fatalf("outcome = %v, want unchanged", outcome)
	}
	got := readCodexConfigForTest(t, configPath)
	if got != before {
		t.Fatalf("config rewritten although the notify line was already present")
	}
	if strings.Count(got, "notify = ") != 1 || strings.Contains(got, codexNotifyMarkerBegin) {
		t.Fatalf("expected the single existing notify key and no marker block:\n%s", got[:300])
	}
	after, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Fatalf("file was touched despite no change")
	}
}

func TestCodexHooksInstall_AlreadyInstalledBlockIsLeftAlone(t *testing.T) {
	configPath := useTempCodexHome(t)
	before := largeCodexConfig([]string{codexNotifyMarkerBegin, codexNotifyLine, codexNotifyMarkerEnd})
	writeCodexConfigForTest(t, configPath, before)

	outcome, err := installCodexNotifyHook(configPath)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if outcome != codexInstallUnchanged {
		t.Fatalf("outcome = %v, want unchanged", outcome)
	}
	if got := readCodexConfigForTest(t, configPath); got != before {
		t.Fatalf("config rewritten although the block was already current")
	}
}

func TestCodexHooksInstall_MissingConfigCreatesBlockOnly(t *testing.T) {
	configPath := useTempCodexHome(t)

	outcome, err := installCodexNotifyHook(configPath)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if outcome != codexInstallInstalled {
		t.Fatalf("outcome = %v, want installed", outcome)
	}
	if got := readCodexConfigForTest(t, configPath); got != codexNotifyBlockText() {
		t.Fatalf("got %q, want the bare block", got)
	}
}

func TestCodexHooksInstall_UnreadableConfigIsNeverTreatedAsEmpty(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	configPath := useTempCodexHome(t)
	before := largeCodexConfig(nil)
	writeCodexConfigForTest(t, configPath, before)
	if err := os.Chmod(configPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(configPath, 0o600) })

	if _, err := installCodexNotifyHook(configPath); err == nil {
		t.Fatalf("expected a read error, got success")
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readCodexConfigForTest(t, configPath); got != before {
		t.Fatalf("unreadable config was overwritten:\n%s", got)
	}
}

func TestCodexHooksInstall_ConflictingNotifyLeavesFileUntouched(t *testing.T) {
	configPath := useTempCodexHome(t)
	before := largeCodexConfig([]string{`notify = ["my-own-notifier"]`})
	writeCodexConfigForTest(t, configPath, before)

	_, err := installCodexNotifyHook(configPath)
	if !errors.Is(err, errCodexNotifyConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
	if got := readCodexConfigForTest(t, configPath); got != before {
		t.Fatalf("config changed on conflict")
	}
}

func TestCodexHooksInstall_MalformedBlockRefusesToWrite(t *testing.T) {
	configPath := useTempCodexHome(t)
	before := largeCodexConfig([]string{codexNotifyMarkerBegin, codexNotifyLine}) // no END marker
	writeCodexConfigForTest(t, configPath, before)

	_, err := installCodexNotifyHook(configPath)
	if !errors.Is(err, errCodexNotifyMalformed) {
		t.Fatalf("err = %v, want malformed", err)
	}
	if got := readCodexConfigForTest(t, configPath); got != before {
		t.Fatalf("config changed on malformed block")
	}
}

func TestCodexHooksInstall_WriteIsAtomic(t *testing.T) {
	configPath := useTempCodexHome(t)
	writeCodexConfigForTest(t, configPath, largeCodexConfig(nil))
	if _, err := installCodexNotifyHook(configPath); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(configPath) {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestCodexConfigLinesPreserved(t *testing.T) {
	before := "a = 1\n# BEGIN AGENTDECK CODEX NOTIFY\nnotify = [\"agent-deck\", \"codex-notify\"]\n# END AGENTDECK CODEX NOTIFY\n[t]\nb = 2\n"
	if !codexConfigLinesPreserved(before, "a = 1\n[t]\nb = 2\n") {
		t.Fatalf("dropping only the owned block must count as preserved")
	}
	if codexConfigLinesPreserved(before, "a = 1\n") {
		t.Fatalf("dropping a user line must be detected")
	}
}
