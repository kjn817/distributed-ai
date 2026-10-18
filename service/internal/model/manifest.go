// Package model handles model manifests and file integrity.
//
// Acceptance rule (M1): a model file is only treated as valid after its
// exact size AND SHA-256 match the manifest. Magic-byte checks alone are
// not sufficient.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is the on-disk model descriptor. Field names follow
// docs/manifest-spec.md.
type Manifest struct {
	ModelID   string `yaml:"model_id" json:"model_id"`
	Version   int    `yaml:"version" json:"version"`
	Format    string `yaml:"format" json:"format"`
	Quant     string `yaml:"quant" json:"quant"`
	SizeBytes int64  `yaml:"size_bytes" json:"size_bytes"`
	SHA256    string `yaml:"sha256" json:"sha256"`
	Source    struct {
		Type string `yaml:"type" json:"type"`
		Repo string `yaml:"repo" json:"repo"`
		File string `yaml:"file" json:"file"`
	} `yaml:"source" json:"source"`
	License    string `yaml:"license" json:"license"`
	LicenseURL string `yaml:"license_url" json:"license_url"`
	Execution  struct {
		Strategies []string `yaml:"strategies" json:"strategies"`
	} `yaml:"execution" json:"execution"`
	// Replication is the desired holder count policy (see docs/manifest-spec.md).
	Replication struct {
		Min    int `yaml:"min" json:"min"`
		Target int `yaml:"target" json:"target"`
	} `yaml:"replication" json:"replication"`
	// Chunks pins the 64MB storage/transfer units (see docs/manifest-spec.md).
	// A chunk is NOT a compute shard — compute placement is a separate layer.
	Chunks ChunkList `yaml:"chunks,omitempty" json:"chunks,omitempty"`
}

// ChunkList describes the fixed-size chunks of a model file.
type ChunkList struct {
	SizeBytes int64       `yaml:"size_bytes" json:"size_bytes"`
	List      []ChunkInfo `yaml:"list" json:"list"`
}

// ChunkInfo pins one chunk's bytes.
type ChunkInfo struct {
	Index     int    `yaml:"index" json:"index"`
	SizeBytes int64  `yaml:"size_bytes" json:"size_bytes"`
	SHA256    string `yaml:"sha256" json:"sha256"`
}

// Load reads a manifest YAML file.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.ModelID == "" || m.SHA256 == "" || m.SizeBytes <= 0 {
		return nil, fmt.Errorf("model: manifest %s missing required fields", path)
	}
	return &m, nil
}

// LoadDir loads every *.yaml manifest in dir. A missing dir is not an
// error — it just means no models are registered (no mock data).
func LoadDir(dir string) ([]*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		m, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Verify checks that filePath has exactly the manifest's size and SHA-256.
// Only a passing file may be treated as a valid model.
func Verify(m *Manifest, filePath string) error {
	st, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("model: %w", err)
	}
	if st.Size() != m.SizeBytes {
		return fmt.Errorf("model: size mismatch: got %d, want %d", st.Size(), m.SizeBytes)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("model: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("model: hash: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != strings.ToLower(m.SHA256) {
		return fmt.Errorf("model: sha256 mismatch")
	}
	return nil
}
