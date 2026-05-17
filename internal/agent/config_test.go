package agent_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"autosk/internal/agent"
)

func writeConfig(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, ".autosk", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_Roundtrip(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "developer", `
model = "sonnet:high"
thinking = "high"
system_prompt = """
You are the developer agent.
"""
extra_args = ["--no-tool", "web_fetch"]
`)
	cfg, err := agent.Load(root, "developer")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "sonnet:high" || cfg.Thinking != "high" {
		t.Fatalf("model/thinking: %+v", cfg)
	}
	if len(cfg.ExtraArgs) != 2 || cfg.ExtraArgs[0] != "--no-tool" {
		t.Fatalf("extra_args: %+v", cfg.ExtraArgs)
	}
	if cfg.SystemPrompt == "" {
		t.Fatalf("system_prompt empty")
	}
}

func TestLoad_Missing(t *testing.T) {
	root := t.TempDir()
	_, err := agent.Load(root, "nobody")
	if !errors.Is(err, agent.ErrConfigNotFound) {
		t.Fatalf("want ErrConfigNotFound, got %v", err)
	}
}

func TestLoad_UnknownKeyRejected(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "dev", `
model = "x"
some_unknown_key = 1
`)
	_, err := agent.Load(root, "dev")
	if err == nil {
		t.Fatal("want error on unknown key")
	}
}

func TestLoad_BadThinkingRejected(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "dev", `
thinking = "extreme"
`)
	_, err := agent.Load(root, "dev")
	if err == nil {
		t.Fatal("want error on bad thinking value")
	}
}

func TestConfigPath(t *testing.T) {
	root := "/proj"
	got := agent.ConfigPath(root, "dev")
	want := "/proj/.autosk/agents/dev.toml"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
