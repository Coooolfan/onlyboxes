package configsource

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	configFileName   = "config.toml"
	configFileEnvKey = "WORKER_CONFIG_FILE"
	envPrefix        = "WORKER_"
)

// Source resolves environment variables before values declared in config.toml.
type Source struct {
	path   string
	values map[string]string
}

func New() Source {
	path, explicit := configFilePath()
	if path == "" {
		return Source{values: map[string]string{}}
	}
	raw := map[string]any{}
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		if os.IsNotExist(err) && !explicit {
			return Source{values: map[string]string{}}
		}
		log.Fatalf("failed to load config file %s: %v", path, err)
	}
	return Source{path: path, values: flatten(raw)}
}

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

func (s Source) Path() string { return s.path }

func (s Source) Get(envKey string) string {
	if value, ok := os.LookupEnv(envKey); ok {
		return value
	}
	return s.values[strings.ToLower(strings.TrimPrefix(envKey, envPrefix))]
}

func (s Source) GetWithEnvAliases(envKey string, aliases ...string) string {
	if value, ok := os.LookupEnv(envKey); ok {
		return value
	}
	for _, alias := range aliases {
		if value, ok := os.LookupEnv(alias); ok {
			return value
		}
	}
	return s.values[strings.ToLower(strings.TrimPrefix(envKey, envPrefix))]
}

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
		encoded := make([]string, 0, len(typed))
		for _, item := range typed {
			itemValue, ok := encodeValue(item)
			if !ok {
				return "", false
			}
			encoded = append(encoded, itemValue)
		}
		payload, err := json.Marshal(encoded)
		return string(payload), err == nil
	case map[string]any:
		encoded := make(map[string]string, len(typed))
		for key, rawValue := range typed {
			if itemValue, ok := encodeValue(rawValue); ok {
				encoded[key] = itemValue
			}
		}
		payload, err := json.Marshal(encoded)
		return string(payload), err == nil
	default:
		return "", false
	}
}
