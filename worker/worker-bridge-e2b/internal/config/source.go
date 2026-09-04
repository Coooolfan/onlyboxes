package config

import "github.com/onlyboxes/onlyboxes/worker/internal/configsource"

const (
	configFileName   = "config.toml"
	configFileEnvKey = "WORKER_CONFIG_FILE"
)

type source struct{ configsource.Source }

func newSource() source                { return source{Source: configsource.New()} }
func (s source) get(key string) string { return s.Get(key) }
func (s source) getWithEnvAliases(key string, aliases ...string) string {
	return s.GetWithEnvAliases(key, aliases...)
}
