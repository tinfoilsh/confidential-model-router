package main

import (
	_ "embed"

	"gopkg.in/yaml.v2"
)

type modelConfig struct {
	Repo         string   `yaml:"repo"`
	EnclaveHosts []string `yaml:"enclaves"`
}

type config struct {
	Models map[string]modelConfig `json:"models"`
}

//go:embed config.yml
var configFile []byte

func loadConfig() (*config, error) {
	var c config
	err := yaml.Unmarshal(configFile, &c)
	return &c, err
}
