// SPDX-FileCopyrightText: Copyright (c) 2023, CIQ, Inc. All rights reserved
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/distribution/distribution/v3/configuration"
)

const (
	DefaultConfigDir = "/etc/beskar"
	BeskarConfigFile = "beskar.yaml"
)

//go:embed default/beskar.yaml
var defaultBeskarConfig string

type Cache struct {
	Addr string `yaml:"addr"`
	Size uint32 `yaml:"size"`
}

type Gossip struct {
	Addr  string   `yaml:"addr"`
	Key   string   `yaml:"key"`
	Peers []string `yaml:"peers"`
}

type PluginMTLS struct {
	Enabled bool   `yaml:"enabled"`
	CA      string `yaml:"ca-cert"`
	CAKey   string `yaml:"ca-key"`
}

type PluginBackend struct {
	URL  string     `yaml:"url"`
	MTLS PluginMTLS `yaml:"mtls"`
}

type Plugin struct {
	Prefix    string          `yaml:"prefix"`
	Mediatype string          `yaml:"mediatype"`
	Backends  []PluginBackend `yaml:"backends"`
}

type BeskarConfig struct {
	Version   string                       `yaml:"version"`
	Profiling bool                         `yaml:"profiling"`
	Cache     Cache                        `yaml:"cache"`
	Gossip    Gossip                       `yaml:"gossip"`
	Plugins   map[string]Plugin            `yaml:"plugins"`
	Registry  *configuration.Configuration `yaml:"registry"`
}

func (bc *BeskarConfig) RunInKubernetes() bool {
	return os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}

type BeskarConfigV1 BeskarConfig

func ParseBeskarConfig(dir string) (*BeskarConfig, error) {
	inMemoryConfig := false
	customDir := false
	filename := filepath.Join(DefaultConfigDir, BeskarConfigFile)
	if dir != "" {
		filename = filepath.Join(dir, BeskarConfigFile)
		customDir = true
	}

	var configReader io.Reader

	f, err := os.Open(filename)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || customDir {
			return nil, err
		}
		configReader = strings.NewReader(defaultBeskarConfig)
		inMemoryConfig = true
	} else {
		defer f.Close()
		configReader = f
	}

	configBuffer := new(bytes.Buffer)
	if _, err := io.Copy(configBuffer, configReader); err != nil {
		return nil, err
	}

	beskarConfig := new(BeskarConfig)

	configParser := configuration.NewParser("beskar", []configuration.VersionedParseInfo{
		{
			Version: configuration.MajorMinorVersion(1, 0),
			ParseAs: reflect.TypeOf(BeskarConfigV1{}),
			ConversionFunc: func(c interface{}) (interface{}, error) {
				if v1, ok := c.(*BeskarConfigV1); ok {
					if v1.Registry.Log.Level == configuration.Loglevel("") {
						//nolint:staticcheck // legacy behavior
						if v1.Registry.Loglevel != configuration.Loglevel("") {
							v1.Registry.Log.Level = v1.Registry.Loglevel
						} else {
							v1.Registry.Log.Level = configuration.Loglevel("info")
						}
					}
					//nolint:staticcheck // legacy behavior
					if v1.Registry.Loglevel != configuration.Loglevel("") {
						v1.Registry.Loglevel = configuration.Loglevel("")
					}

					if v1.Registry.Catalog.MaxEntries <= 0 {
						v1.Registry.Catalog.MaxEntries = 1000
					}

					if v1.Registry.Storage.Type() == "" {
						return nil, errors.New("no storage configuration provided")
					} else if inMemoryConfig && v1.Registry.Storage.Type() == "filesystem" {
						params := v1.Registry.Storage.Parameters()
						params["rootdirectory"] = "/tmp/beskar-registry"
					}

					if v1.Cache.Size == 0 {
						v1.Cache.Size = 64
					}

					if v1.Gossip.Key == "" {
						return nil, fmt.Errorf("gossip key is missing")
					}

					return (*BeskarConfig)(v1), nil
				}
				return nil, fmt.Errorf("expected *BeskarConfigV1, received %#v", c)
			},
		},
	})

	if err := configParser.Parse(configBuffer.Bytes(), beskarConfig); err != nil {
		return nil, err
	}

	return beskarConfig, nil
}
