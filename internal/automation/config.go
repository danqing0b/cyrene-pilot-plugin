// SPDX-License-Identifier: Apache-2.0
// Package automation owns plugin workflows; Product services retain resource authority.
package automation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxBytes = 16 << 20
const MaxReply = 2 << 20

var ID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type Endpoint struct {
	URL       string `json:"url"`
	TokenFile string `json:"tokenFile,omitempty"`
	token     string
}
type Model struct {
	Endpoint
	Model          string `json:"model"`
	Revision       string `json:"revision"`
	MaxTokens      int    `json:"maxTokens"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	SystemPrompt   string `json:"systemPrompt,omitempty"`
}
type CatalystTarget struct {
	DatasetID       string `json:"datasetId"`
	RevisionID      string `json:"revisionId"`
	ResourceVersion int    `json:"resourceVersion"`
}
type Dataset struct {
	Path     string          `json:"path"`
	Digest   string          `json:"digest"`
	Catalyst *CatalystTarget `json:"catalyst,omitempty"`
}
type EchoConfig struct {
	Endpoint
	ArtifactRoot    string `json:"artifactRoot"`
	SuiteID         string `json:"suiteId"`
	EngineBindingID string `json:"engineBindingId"`
}
type GuardConfig struct {
	AIModelID     string `json:"aiModelId,omitempty"`
	AllowTextToAI bool   `json:"allowTextToAI"`
	MaxAICalls    int    `json:"maxAICalls"`
}
type YieldConfig struct {
	Endpoint
	WorkspaceID          string   `json:"workspaceId"`
	MaxRepairStarts      int      `json:"maxRepairStarts"`
	MaxSteps             int      `json:"maxSteps"`
	AIModelID            string   `json:"aiModelId,omitempty"`
	AllowedRunIDs        []string `json:"allowedRunIds"`
	AllowDiagnosticsToAI bool     `json:"allowDiagnosticsToAI"`
}
type Config struct {
	Models    map[string]Model   `json:"models"`
	Datasets  map[string]Dataset `json:"datasets"`
	Echo      *EchoConfig        `json:"echo,omitempty"`
	Catalyst  *Endpoint          `json:"catalyst,omitempty"`
	DataGuard GuardConfig        `json:"dataGuard"`
	Yield     *YieldConfig       `json:"yield,omitempty"`
	MaxRows   int                `json:"maxRows"`
	MaxJobs   int                `json:"maxJobs"`
}

func Digest(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

func Strict(raw []byte, out any) error {
	if err := uniqueJSON(raw); err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("invalid JSON arguments")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func uniqueJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("JSON must be valid UTF-8")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting limit exceeded")
		}
		token, err := d.Token()
		if err != nil {
			return errors.New("invalid JSON")
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return errors.New("invalid JSON key")
					}
					name, ok := key.(string)
					if !ok || seen[name] {
						return errors.New("duplicate or invalid JSON key")
					}
					seen[name] = true
					if err := value(depth + 1); err != nil {
						return err
					}
				}
				if end, err := d.Token(); err != nil || end != json.Delim('}') {
					return errors.New("invalid JSON object")
				}
			case '[':
				for d.More() {
					if err := value(depth + 1); err != nil {
						return err
					}
				}
				if end, err := d.Token(); err != nil || end != json.Delim(']') {
					return errors.New("invalid JSON array")
				}
			default:
				return errors.New("invalid JSON delimiter")
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func (e *Endpoint) validate() error {
	u, err := url.Parse(e.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("endpoint must be a fixed origin")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return errors.New("endpoint requires HTTPS or loopback HTTP")
	}
	e.URL = strings.TrimSuffix(e.URL, "/")
	if e.TokenFile != "" {
		f, err := os.Open(e.TokenFile)
		if err != nil {
			return errors.New("credential file unavailable")
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("credential must be a private regular file")
		}
		b, err := io.ReadAll(io.LimitReader(f, 4097))
		if err != nil {
			return err
		}
		e.token = strings.TrimSpace(string(b))
		if len(b) > 4096 || len(e.token) < 16 || strings.ContainsAny(e.token, " \r\n\t") {
			return errors.New("invalid credential")
		}
	}
	return nil
}

func (c *Config) Validate() error {
	if c.MaxRows == 0 {
		c.MaxRows = 500
	}
	if c.MaxJobs == 0 {
		c.MaxJobs = 1000
	}
	if c.MaxRows < 1 || c.MaxRows > 10000 || c.MaxJobs < 1 || c.MaxJobs > 10000 {
		return errors.New("invalid workflow limits")
	}
	for id, m := range c.Models {
		if !ID.MatchString(id) || strings.TrimSpace(m.Model) == "" || len(m.Model) > 200 || strings.TrimSpace(m.Revision) == "" || len(m.Revision) > 200 || len(m.SystemPrompt) > 8192 {
			return errors.New("model identity and revision are required")
		}
		if m.MaxTokens == 0 {
			m.MaxTokens = 1024
		}
		if m.TimeoutSeconds == 0 {
			m.TimeoutSeconds = 60
		}
		if m.MaxTokens < 1 || m.MaxTokens > 8192 || m.TimeoutSeconds < 1 || m.TimeoutSeconds > 120 {
			return errors.New("invalid model limits")
		}
		if err := m.Endpoint.validate(); err != nil {
			return fmt.Errorf("model %s: %w", id, err)
		}
		c.Models[id] = m
	}
	for id, d := range c.Datasets {
		if !ID.MatchString(id) || !filepath.IsAbs(d.Path) || !digestRE.MatchString(d.Digest) {
			return errors.New("datasets require an ID, absolute path and sha256 digest")
		}
		if d.Catalyst != nil && (!ID.MatchString(d.Catalyst.DatasetID) || !ID.MatchString(d.Catalyst.RevisionID) || d.Catalyst.ResourceVersion < 1 || c.Catalyst == nil) {
			return errors.New("invalid Catalyst target")
		}
	}
	if c.Echo != nil {
		if err := c.Echo.Endpoint.validate(); err != nil {
			return err
		}
		if !filepath.IsAbs(c.Echo.ArtifactRoot) || !ID.MatchString(c.Echo.SuiteID) || !ID.MatchString(c.Echo.EngineBindingID) {
			return errors.New("Echo requires an artifact root, suite and engine binding")
		}
	}
	if c.Catalyst != nil {
		if err := c.Catalyst.validate(); err != nil {
			return err
		}
	}
	if c.DataGuard.AIModelID != "" {
		if _, ok := c.Models[c.DataGuard.AIModelID]; !ok || !c.DataGuard.AllowTextToAI {
			return errors.New("data guard AI requires a registered model and explicit host text permission")
		}
	}
	if c.DataGuard.MaxAICalls == 0 {
		c.DataGuard.MaxAICalls = 100
	}
	if c.DataGuard.MaxAICalls < 1 || c.DataGuard.MaxAICalls > 1000 {
		return errors.New("invalid data guard AI call limit")
	}
	if c.Yield != nil {
		if err := c.Yield.Endpoint.validate(); err != nil {
			return err
		}
		if !ID.MatchString(c.Yield.WorkspaceID) || c.Yield.MaxRepairStarts < 0 || c.Yield.MaxRepairStarts > 20 || c.Yield.MaxSteps < 1 || c.Yield.MaxSteps > 100000 {
			return errors.New("Yield requires workspace and bounded repair budget")
		}
		if c.Yield.AIModelID != "" {
			if _, ok := c.Models[c.Yield.AIModelID]; !ok || !c.Yield.AllowDiagnosticsToAI {
				return errors.New("diagnosis AI requires a model and host permission to send redacted diagnostics")
			}
		}
		for _, id := range c.Yield.AllowedRunIDs {
			if !ID.MatchString(id) {
				return errors.New("invalid allowed run ID")
			}
		}
	}
	return nil
}

func (c Config) fingerprint() string { b, _ := json.Marshal(c); return Digest(b) }

func readDataset(d Dataset, maxRows int) ([]map[string]any, error) {
	f, err := os.Open(d.Path)
	if err != nil {
		return nil, errors.New("dataset unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxBytes {
		return nil, errors.New("dataset must be a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil || len(b) > MaxBytes || Digest(b) != d.Digest {
		return nil, errors.New("dataset digest mismatch or size exceeded")
	}
	rows := []map[string]any{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row map[string]any
		if err := uniqueJSON([]byte(line)); err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&row); err != nil || row == nil {
			return nil, errors.New("dataset contains an invalid JSON object")
		}
		rows = append(rows, row)
		if len(rows) > maxRows {
			return nil, errors.New("dataset row limit exceeded")
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("dataset is empty")
	}
	return rows, nil
}
