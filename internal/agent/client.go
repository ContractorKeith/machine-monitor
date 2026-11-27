package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

type Client struct {
	HubURL    string `json:"hubUrl"`
	Key       string `json:"key"`
	DeviceKey string `json:"deviceKey,omitempty"`
}

var ErrNotPaired = errors.New("this machine is not paired with a hub: run machine-monitor connect URL")

func ValidKey(s string) bool { return keyRE.MatchString(s) }

func LoadClient(path string) (Client, error) {
	var c Client
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, ErrNotPaired
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func SaveClient(path string, c Client) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFile0600(path, append(b, '\n'))
}

var ErrKeyInURL = errors.New("the hub URL carries a key (?key=); a key in a URL ends up in process lists and unit files. " +
	"Pair instead, with: echo KEY | machine-monitor connect --key-stdin URL")
