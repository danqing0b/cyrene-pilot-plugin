// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/danqing0b/cyrene-pilot-plugin/internal/automation"
)

const pluginID = "cyrene.tools.release-pilot"
const pluginVersion = "0.2.0"
const pilotAlias = "release-pilot"
const maxBody = 2_000_000

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type artifact struct {
	URI            string `json:"uri"`
	Digest         string `json:"digest"`
	Size           int64  `json:"size_bytes"`
	Kind           string `json:"kind"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
}

type connection struct {
	URL       string `json:"url"`
	TokenFile string `json:"tokenFile,omitempty"`
	token     string
}

type baselineConfig struct {
	InputID string `json:"inputId"`
	Digest  string `json:"digest"`
	RouteID string `json:"routeId"`
	Alias   string `json:"alias"`
	Model   string `json:"model"`
}

type candidate struct {
	InputID         string   `json:"inputId"`
	Model           artifact `json:"artifact"`
	ServingBinding  string   `json:"servingBindingId"`
	ProviderBinding string   `json:"providerBindingId"`
}

type config struct {
	Automation     *automation.Config   `json:"automation,omitempty"`
	Echo           connection           `json:"echo"`
	Reactor        connection           `json:"reactor"`
	Exchange       connection           `json:"exchange"`
	Inference      connection           `json:"inference"`
	AdminTokenFile string               `json:"adminTokenFile"`
	PilotTokenFile string               `json:"pilotTokenFile"`
	HTTPListen     string               `json:"httpListen"`
	ReceiverID     string               `json:"receiverId"`
	GatewayID      string               `json:"gatewayEndpointId"`
	SuiteID        string               `json:"evaluationSuiteId"`
	Baseline       baselineConfig       `json:"baseline"`
	Candidates     map[string]candidate `json:"candidates"`
	adminToken     string
	pilotToken     string
}

func strictJSON(r io.Reader, out any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func readSecret(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("token files must be private regular files")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 32 || len(raw) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("invalid bearer token")
	}
	return token, nil
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *connection) load(requireToken bool) error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) ||
		(u.Path != "" && u.Path != "/") {
		return errors.New("service URL must be a fixed HTTPS or loopback HTTP origin")
	}
	c.URL = strings.TrimSuffix(c.URL, "/")
	if c.TokenFile == "" && !requireToken {
		return nil
	}
	c.token, err = readSecret(c.TokenFile)
	return err
}

func loadConfig(path string) (config, error) {
	var c config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	if err := strictJSON(io.LimitReader(f, maxBody), &c); err != nil {
		return c, err
	}
	if c.Automation != nil {
		if err := c.Automation.Validate(); err != nil {
			return c, err
		}
		if len(c.Candidates) == 0 {
			host, _, err := net.SplitHostPort(c.HTTPListen)
			if err != nil || !isLoopback(host) {
				return c, errors.New("httpListen must bind a loopback IP address")
			}
			c.adminToken, err = readSecret(c.AdminTokenFile)
			return c, err
		}
	}
	for _, item := range []struct {
		name         string
		value        *connection
		requireToken bool
	}{
		{"echo", &c.Echo, false}, {"reactor", &c.Reactor, true},
		{"exchange", &c.Exchange, true}, {"inference", &c.Inference, true},
	} {
		if err := item.value.load(item.requireToken); err != nil {
			return c, fmt.Errorf("%s: %w", item.name, err)
		}
	}
	host, _, err := net.SplitHostPort(c.HTTPListen)
	if err != nil || !isLoopback(host) {
		return c, errors.New("httpListen must bind a loopback IP address")
	}
	for _, id := range []string{c.ReceiverID, c.GatewayID, c.SuiteID, c.Baseline.RouteID, c.Baseline.InputID, c.Baseline.Alias} {
		if !identifier.MatchString(id) {
			return c, errors.New("invalid configured resource identifier")
		}
	}
	if !digestPattern.MatchString(c.Baseline.Digest) || c.Baseline.Model == "" ||
		c.Baseline.Alias == pilotAlias || c.Baseline.Alias == "release-pilot-"+c.Baseline.RouteID || len(c.Candidates) == 0 {
		return c, errors.New("baseline and candidates are required")
	}
	for id, item := range c.Candidates {
		a := item.Model
		if !identifier.MatchString(id) || len(id) > 60 ||
			!identifier.MatchString(item.InputID) || item.ServingBinding == "" ||
			item.ProviderBinding == "" || a.Kind != "model" || a.Size <= 0 ||
			!digestPattern.MatchString(a.Digest) || a.Digest == c.Baseline.Digest ||
			a.URI != "artifact://sha256/"+strings.TrimPrefix(a.Digest, "sha256:") ||
			(a.ManifestDigest != "" && !digestPattern.MatchString(a.ManifestDigest)) {
			return c, fmt.Errorf("invalid candidate %q", id)
		}
	}
	if c.adminToken, err = readSecret(c.AdminTokenFile); err != nil {
		return c, err
	}
	if c.pilotToken, err = readSecret(c.PilotTokenFile); err != nil {
		return c, err
	}
	if c.adminToken == c.pilotToken {
		return c, errors.New("operator and inference tokens must differ")
	}
	return c, nil
}
