package main

import (
	"path/filepath"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
)

func TestLoadPairingPreservesDeviceKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	want := agent.Client{HubURL: "http://localhost:7777", Key: "shared-token", DeviceKey: "private-device-key"}
	if err := agent.SaveClient(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadPairing(path)
	if err != nil || got != want {
		t.Fatalf("pairing credentials did not round trip: %v", err)
	}
}
