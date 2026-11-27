package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

type Config struct {
	Hub       string `json:"hub"`
	Key       string `json:"key"`
	DeviceKey string `json:"deviceKey,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var raw struct {
		Hub       string `json:"hub"`
		HubURL    string `json:"hubUrl"`
		Key       string `json:"key"`
		Token     string `json:"token"`
		DeviceKey string `json:"deviceKey"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	c := Config{Hub: raw.Hub, Key: raw.Key, DeviceKey: raw.DeviceKey}
	if c.Hub == "" {
		c.Hub = raw.HubURL
	}
	if c.Key == "" {
		c.Key = raw.Token
	}
	return c, nil
}

func SaveConfig(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFile0600(path, b)
}
