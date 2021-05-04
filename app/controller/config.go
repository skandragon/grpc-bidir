package main

/*
 * Copyright 2021 OpsMx, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License")
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"

	"gopkg.in/yaml.v3"

	"github.com/opsmx/oes-birger/pkg/ca"
)

// ControllerConfig holds all the configuration for the controller.  The
// configuration file is loaded from disk first, and then any
// environment variables are applied.
type ControllerConfig struct {
	Agents                  map[string]*agentConfig `yaml:"agents,omitempty"`
	ServiceAuth             serviceAuthConfig       `yaml:"serviceAuth,omitempty"`
	Webhook                 string                  `yaml:"webhook,omitempty"`
	ServerNames             []string                `yaml:"serverNames,omitempty"`
	CAConfig                ca.Config               `yaml:"caConfig,omitempty"`
	PrometheusListenPort    uint16                  `yaml:"prometheusListenPort"`
	ServiceHostname         *string                 `yaml:"serviceHostname"`
	ServiceListenPort       uint16                  `yaml:"serviceListenPort"`
	ControlHostname         *string                 `yaml:"controlHostname"`
	ControlListenPort       uint16                  `yaml:"controlListenPort"`
	AgentHostname           *string                 `yaml:"agentHostname"`
	AgentListenPort         uint16                  `yaml:"agentListenPort"`
	AgentAdvertisePort      uint16                  `yaml:"agentAdvertisePort"`
	RemoteCommandHostname   *string                 `yaml:"remoteCommandHostname"`
	RemoteCommandListenPort uint16                  `yaml:"remoteCommandListenPort"`
}

type agentConfig struct {
	Name string `yaml:"name,omitempty"`
}

type serviceAuthConfig struct {
	CurrentKeyName string `yaml:"currentKeyName,omitempty"`
}

// LoadConfig will load YAML configuration from the provided filename,
// and then apply environment variables to override some subset of
// available options.
func LoadConfig(f io.Reader) (*ControllerConfig, error) {
	buf, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}

	config := &ControllerConfig{}
	err = yaml.Unmarshal(buf, config)
	if err != nil {
		return nil, err
	}

	if config.AgentListenPort == 0 {
		config.AgentListenPort = 9001
	}
	if config.AgentAdvertisePort == 0 {
		config.AgentAdvertisePort = config.AgentListenPort
	}
	if config.AgentHostname == nil {
		return nil, fmt.Errorf("agentHostname not set")
	}

	if config.ServiceListenPort == 0 {
		config.ServiceListenPort = 9002
	}
	if config.ServiceHostname == nil {
		return nil, fmt.Errorf("serviceHostname not set")
	}

	if config.ControlListenPort == 0 {
		config.ControlListenPort = 9003
	}
	if config.ControlHostname == nil {
		return nil, fmt.Errorf("controlHostname not set")
	}

	if config.RemoteCommandListenPort == 0 {
		config.RemoteCommandListenPort = 9004
	}
	if config.RemoteCommandHostname == nil {
		return nil, fmt.Errorf("remoteCommandHostname not set")
	}

	if config.PrometheusListenPort == 0 {
		config.PrometheusListenPort = 9102
	}

	config.addAllHostnames()

	return config, nil
}

func (c *ControllerConfig) hasServerName(target string) bool {
	for _, a := range c.ServerNames {
		if a == target {
			return true
		}
	}
	return false
}

func (c *ControllerConfig) addIfMissing(target *string, reason string) {
	if target != nil && !c.hasServerName(*target) {
		c.ServerNames = append(c.ServerNames, *target)
		log.Printf("Adding %s to ServerNames (for %s configuration setting)", *target, reason)
	}
}

func (c *ControllerConfig) addAllHostnames() {
	c.addIfMissing(c.AgentHostname, "agentHostname")
	c.addIfMissing(c.ControlHostname, "commandHostname")
	c.addIfMissing(c.ServiceHostname, "ServiceBaseHostname")
	c.addIfMissing(c.RemoteCommandHostname, "cmdToolHostname")
}

func (c *ControllerConfig) getServiceURL() string {
	return fmt.Sprintf("https://%s:%d", *c.ServiceHostname, c.ServiceListenPort)
}

func (c *ControllerConfig) getControlURL() string {
	return fmt.Sprintf("https://%s:%d", *c.ControlHostname, c.ControlListenPort)
}

//
// Dump will display MOST of the controller's configuration.
//
func (c *ControllerConfig) Dump() {
	log.Println("ControllerConfig:")
	log.Printf("ServerNames:")
	for _, n := range config.ServerNames {
		log.Printf("  %s", n)
	}
	log.Printf("Service hostname: %s, port: %d",
		*c.ServiceHostname, c.ServiceListenPort)
	log.Printf("URL returned for kubectl components: %s",
		c.getServiceURL())
	log.Printf("Agent hostname: %s, port %d (advertised %d)",
		*c.AgentHostname, c.AgentListenPort, c.AgentAdvertisePort)
	log.Printf("Control hostname: %s, port %d",
		*c.ControlHostname, c.ControlListenPort)
	log.Printf("RemoteCommand hostname: %s, port %d",
		*c.RemoteCommandHostname, c.RemoteCommandListenPort)
}
