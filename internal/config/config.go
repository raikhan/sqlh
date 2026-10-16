package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/raikhan/sqlh/internal/database"
	"gopkg.in/yaml.v2"
)

var (
	ErrNotFoundConfig = errors.New("NotFound Config")
)

var (
	YamlConfigPath = configFilePath("config.yml")
)

type Config struct {
	LowercaseKeywords bool `json:"lowercaseKeywords" yaml:"lowercaseKeywords"`
	// LowercaseIdentifiers inserts all-upper-case identifiers (Oracle's stored
	// form of unquoted names) in lower case. Mixed-case names are left alone,
	// since those need exact (quoted) spelling.
	LowercaseIdentifiers bool                 `json:"lowercaseIdentifiers" yaml:"lowercaseIdentifiers"`
	Connections          []*database.DBConfig `json:"connections" yaml:"connections"`
	Results              ResultsConfig        `json:"results" yaml:"results"`
	// Diagnostics: underline unknown tables, aliases and columns (checked
	// against the schema cache, never the database). Default: true.
	Diagnostics *bool `json:"diagnostics" yaml:"diagnostics"`
	// RefreshAfterDDL: reload the schema cache after a run that executed DDL
	// (CREATE, ALTER, DROP, RENAME, COMMENT). Default: true.
	RefreshAfterDDL *bool `json:"refreshAfterDDL" yaml:"refreshAfterDDL"`
	// Relations: the table picker and join completion.
	Relations RelationsConfig `json:"relations" yaml:"relations"`
}

// RelationsConfig controls how relationships between tables are used.
type RelationsConfig struct {
	// Insert: what the table picker inserts for a related table after JOIN:
	// "join" (table, alias and ON condition; the default) or "name".
	Insert string `json:"insert" yaml:"insert"`
	// Maybe: also offer relationships guessed from column names (customer_id →
	// customer.id, when no foreign key is declared). Default: true.
	Maybe *bool `json:"maybe" yaml:"maybe"`
}

// InsertMode returns "join" or "name".
func (r RelationsConfig) InsertMode() string {
	if strings.EqualFold(r.Insert, "name") {
		return "name"
	}
	return "join"
}

// MaybeEnabled reports whether guessed relationships are shown (the default).
func (r RelationsConfig) MaybeEnabled() bool { return r.Maybe == nil || *r.Maybe }

// DiagnosticsEnabled reports whether diagnostics are on (the default).
func (c *Config) DiagnosticsEnabled() bool { return c.Diagnostics == nil || *c.Diagnostics }

// RefreshAfterDDLEnabled reports whether the schema cache is reloaded after
// DDL runs (the default).
func (c *Config) RefreshAfterDDLEnabled() bool { return c.RefreshAfterDDL == nil || *c.RefreshAfterDDL }

// ResultsConfig controls query result files.
type ResultsConfig struct {
	// Dir is where CSV files are written. Supports {workspace}, {home}, {conn}
	// and "~"; relative paths are relative to the workspace.
	// Default: {workspace}/.sqlh/results
	Dir string `json:"dir" yaml:"dir"`
	// PreviewLimit caps rows for "Run → CSV" and the text view. Default: 100.
	PreviewLimit int `json:"previewLimit" yaml:"previewLimit"`
	// Null is the text written for SQL NULL. Default: empty.
	Null string `json:"null" yaml:"null"`
	// ReuseFor: how long a complete result may stand in for running the same
	// statement again (`space i d` copies it). A duration ("10m", "90s");
	// "0" disables. Default: 10m.
	ReuseFor string `json:"reuseFor" yaml:"reuseFor"`
	// Viewer opens result files (`sqlh open`, `run --open`): the program and
	// its arguments; "{}" stands for the files, else they are appended; a
	// leading "~" in the program is expanded. Default: open (macOS),
	// xdg-open.
	Viewer []string `json:"viewer" yaml:"viewer"`
}

// DefaultReuseFor applies when results.reuseFor is not set.
const DefaultReuseFor = 10 * time.Minute

// ReuseWindow returns the effective results.reuseFor (0: reuse disabled).
func (r ResultsConfig) ReuseWindow() time.Duration {
	if strings.TrimSpace(r.ReuseFor) == "" {
		return DefaultReuseFor
	}
	d, err := parseDuration(r.ReuseFor)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// parseDuration also takes "0" and a bare number of seconds.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

// DefaultPreviewLimit applies when results.previewLimit is not set.
const DefaultPreviewLimit = 100

// Limit returns the effective preview limit.
func (r ResultsConfig) Limit() int {
	if r.PreviewLimit > 0 {
		return r.PreviewLimit
	}
	return DefaultPreviewLimit
}

func (c *Config) Validate() error {
	if r := strings.TrimSpace(c.Results.ReuseFor); r != "" {
		if d, err := parseDuration(r); err != nil || d < 0 {
			return fmt.Errorf("results.reuseFor: %q is not a duration (e.g. 10m, 90s, 0)", r)
		}
	}
	for i, conn := range c.Connections {
		if err := conn.Validate(); err != nil {
			name := conn.Alias
			if name == "" {
				name = fmt.Sprintf("#%d", i+1)
			}
			return fmt.Errorf("connection %s: %w", name, err)
		}
	}
	return nil
}

func NewConfig() *Config {
	cfg := &Config{}
	cfg.LowercaseKeywords = false
	return cfg
}

func GetDefaultConfig() (*Config, error) {
	cfg := NewConfig()
	if err := cfg.Load(YamlConfigPath); err != nil {
		return nil, err
	}
	return cfg, nil
}

func GetConfig(fp string) (*Config, error) {
	cfg := NewConfig()
	expandPath, err := expand(fp)
	if err != nil {
		return nil, err
	}
	if err := cfg.Load(expandPath); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Load(fp string) error {
	if !IsFileExist(fp) {
		return ErrNotFoundConfig
	}

	file, err := os.OpenFile(fp, os.O_RDONLY, 0666)
	if err != nil {
		return fmt.Errorf("cannot open config, %w", err)
	}
	defer file.Close()

	b, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("cannot read config, %w", err)
	}

	if err = yaml.Unmarshal(b, c); err != nil {
		return fmt.Errorf("failed unmarshal yaml, %w, %s", err, string(b))
	}

	if err := c.Validate(); err != nil {
		return fmt.Errorf("failed validation, %w", err)
	}
	return nil
}

func IsFileExist(fPath string) bool {
	_, err := os.Stat(fPath)
	return err == nil || !os.IsNotExist(err)
}

func configFilePath(fileName string) string {
	if xdgConfigHome := os.Getenv("XDG_CONFIG_HOME"); xdgConfigHome != "" {
		return filepath.Join(xdgConfigHome, "sqlh", fileName)
	}

	var configDir string
	if runtime.GOOS == "darwin" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			panic(err)
		}
		configDir = filepath.Join(homeDir, ".config")
	} else {
		var err error
		configDir, err = os.UserConfigDir()
		if err != nil {
			panic(err)
		}
	}

	return filepath.Join(configDir, "sqlh", fileName)
}

func expand(path string) (string, error) {
	if len(path) == 0 || path[0] != '~' {
		return path, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, path[1:]), nil
}
