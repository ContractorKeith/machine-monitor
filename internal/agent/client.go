package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

// Client is client.json: the hub this machine is paired with and its key. The agent and
// the connected TUI both read it.
type Client struct {
	HubURL string `json:"hubUrl"`
	Key    string `json:"key"`
}

// ErrNotPaired means client.json doesn't exist yet.
var ErrNotPaired = errors.New("this machine is not paired with a hub: run machine-monitor connect URL")

// ValidKey reports whether s looks like a hub key.
func ValidKey(s string) bool { return keyRE.MatchString(s) }

// LoadClient reads client.json. A missing file is ErrNotPaired.
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

// SaveClient writes client.json, mode 0600.
func SaveClient(path string, c Client) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFile0600(path, append(b, '\n'))
}

// ErrKeyInURL is why a hub URL with ?key= is refused where it would be stored or shown.
var ErrKeyInURL = errors.New("the hub URL carries a key (?key=); a key in a URL ends up in process lists and unit files. " +
	"Pair instead, with: echo KEY | machine-monitor connect --key-stdin URL")
