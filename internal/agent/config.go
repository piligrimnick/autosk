// Package agent — execution config files.
//
// An agent's executable configuration (model, thinking, system prompt,
// extra pi args) lives outside the DB in .autosk/agents/<name>.toml.
// Tracked in git. Read by the daemon at spawn time.
//
// Format:
//
//	model         = "sonnet:high"
//	thinking      = "high"
//	system_prompt = """
//	You are the developer agent.
//	"""
//	extra_args    = ["--no-tool", "web_fetch"]
//
// All fields are optional. Missing / empty model and thinking mean
// "pi default". Unknown keys are rejected so typos surface fast.
package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the on-disk shape of an agent's execution config.
type Config struct {
	Model        string   `toml:"model"`
	Thinking     string   `toml:"thinking"`
	SystemPrompt string   `toml:"system_prompt"`
	ExtraArgs    []string `toml:"extra_args"`
}

// Validate sanity-checks the parsed config.
func (c Config) Validate() error {
	switch c.Thinking {
	case "", "off", "minimal", "low", "medium", "high", "xhigh":
	default:
		return fmt.Errorf("invalid thinking %q (want one of off|minimal|low|medium|high|xhigh)", c.Thinking)
	}
	return nil
}

// ErrConfigNotFound is returned by Load when the config file is missing.
var ErrConfigNotFound = errors.New("agent config not found")

// Load reads .autosk/agents/<name>.toml relative to projectRoot. Returns
// ErrConfigNotFound when the file is absent.
func Load(projectRoot, name string) (Config, error) {
	if projectRoot == "" {
		return Config{}, fmt.Errorf("project root is empty")
	}
	if name == "" {
		return Config{}, fmt.Errorf("agent name is empty")
	}
	path := ConfigPath(projectRoot, name)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, fmt.Errorf("%w: %s", ErrConfigNotFound, path)
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	meta, err := toml.Decode(string(b), &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return Config{}, fmt.Errorf("parse %s: unknown keys: %v", path, keys)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return cfg, nil
}

// ConfigPath returns the absolute path of an agent's config file relative
// to projectRoot. Does not check existence.
func ConfigPath(projectRoot, name string) string {
	return filepath.Join(projectRoot, ".autosk", "agents", name+".toml")
}
