package config

import "github.com/onlyboxes/onlyboxes/worker/internal/configsource"

const (
	configFileName   = "config.toml"
	configFileEnvKey = "WORKER_CONFIG_FILE"
)

type source struct{ configsource.Source }

func newSource() source { return source{Source: configsource.New()} }
