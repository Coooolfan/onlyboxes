package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	configFileName   = "config.toml"
	configFileEnvKey = "CONSOLE_CONFIG_FILE"
	envPrefix        = "CONSOLE_"
)

// source resolves configuration values from environment variables first and
// falls back to the values declared in config.toml.
type source struct {
	path   string
	values map[string]string
}

func newSource() source {
	path, explicit := configFilePath()
	if path == "" {
		return source{values: map[string]string{}}
	}

	raw := map[string]any{}
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		if os.IsNotExist(err) && !explicit {
			return source{values: map[string]string{}}
		}
		log.Fatalf("failed to load config file %s: %v", path, err)
	}

	return source{path: path, values: flatten(raw)}
}

// configFilePath returns the config file to load and whether it was requested
// explicitly through CONSOLE_CONFIG_FILE.
func configFilePath() (string, bool) {
	if explicit := strings.TrimSpace(os.Getenv(configFileEnvKey)); explicit != "" {
		return explicit, true
	}

	if executable, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		candidate := filepath.Join(filepath.Dir(executable), configFileName)
		if fileExists(candidate) {
			return candidate, false
		}
	}

	if fileExists(configFileName) {
		return configFileName, false
	}

	return "", false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Path returns the loaded config file path, empty when no file was used.
func (s source) Path() string {
	return s.path
}

// lookup reports whether a key is declared in the environment or in the config
// file, mirroring os.LookupEnv semantics (an explicit empty value counts as set).
func (s source) lookup(envKey string) (string, bool) {
	if value, ok := os.LookupEnv(envKey); ok {
		return value, true
	}
	value, ok := s.values[fileKey(envKey)]
	return value, ok
}

// get returns the value for an environment variable key, falling back to the
// matching config file key (env key without the CONSOLE_ prefix, lowercased).
func (s source) get(envKey string) string {
	if value := os.Getenv(envKey); value != "" {
		return value
	}
	return s.values[fileKey(envKey)]
}

func fileKey(envKey string) string {
	return strings.ToLower(strings.TrimPrefix(envKey, envPrefix))
}

// flatten converts TOML values into the same string form accepted by the
// environment variable parsers. Nested tables are additionally exposed as
// `parent_child` keys so that grouped sections map onto flat env names.
func flatten(raw map[string]any) map[string]string {
	values := make(map[string]string, len(raw))
	flattenInto(values, "", raw)
	return values
}

func flattenInto(values map[string]string, prefix string, table map[string]any) {
	for key, value := range table {
		fullKey := strings.ToLower(key)
		if prefix != "" {
			fullKey = prefix + "_" + fullKey
		}
		if nested, ok := value.(map[string]any); ok {
			flattenInto(values, fullKey, nested)
		}
		if encoded, ok := encodeValue(value); ok {
			values[fullKey] = encoded
		}
	}
}

func encodeValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case []any:
		return encodeArray(typed)
	case map[string]any:
		return encodeTable(typed), true
	default:
		return "", false
	}
}

// encodeArray renders TOML arrays as JSON arrays of strings.
func encodeArray(items []any) (string, bool) {
	encoded := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := encodeValue(item)
		if !ok {
			return "", false
		}
		encoded = append(encoded, value)
	}
	payload, err := json.Marshal(encoded)
	if err != nil {
		return "", false
	}
	return string(payload), true
}

// encodeTable renders TOML tables as the `key=value,key=value` CSV form.
func encodeTable(table map[string]any) string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		value, ok := encodeValue(table[key])
		if !ok {
			continue
		}
		entries = append(entries, fmt.Sprintf("%s=%s", key, value))
	}
	return strings.Join(entries, ",")
}
