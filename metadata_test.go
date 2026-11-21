// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPackageMetadataMatchesService(t *testing.T) {
	var manifest struct {
		ID, Version string
		Runtime     struct{ Language string }
	}
	raw, err := os.ReadFile("plugin.manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &manifest) != nil || manifest.ID != pluginID || manifest.Version != pluginVersion || manifest.Runtime.Language != "mixed" {
		t.Fatal("package metadata does not match the running service")
	}
	for _, name := range []string{"configuration.schema.json", "automation.schema.json", "examples/automation.config.json"} {
		b, err := os.ReadFile(name)
		if err != nil || !json.Valid(b) {
			t.Fatalf("invalid metadata: %s %v", name, err)
		}
	}
}
