// Copyright 2019 HAProxy Technologies
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package configuration

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"math/rand"
	"time"

	"github.com/google/renameio"
	"github.com/haproxytech/models/v2"
	log "github.com/sirupsen/logrus"

	petname "github.com/dustinkirkland/golang-petname"
	"gopkg.in/yaml.v2"
)

var cfg *Configuration

type HAProxyConfiguration struct {
	ConfigFile           string `short:"c" long:"config-file" description:"Path to the haproxy configuration file" default:"/etc/haproxy/haproxy.cfg"`
	Userlist             string `short:"u" long:"userlist" description:"Userlist in HAProxy configuration to use for API Basic Authentication" default:"controller"`
	HAProxy              string `short:"b" long:"haproxy-bin" description:"Path to the haproxy binary file" default:"haproxy"`
	ReloadDelay          int    `short:"d" long:"reload-delay" description:"Minimum delay between two reloads (in s)" default:"5"`
	ReloadCmd            string `short:"r" long:"reload-cmd" description:"Reload command"`
	RestartCmd           string `short:"s" long:"restart-cmd" description:"Restart command"`
	ReloadRetention      int    `long:"reload-retention" description:"Reload retention in days, every older reload id will be deleted" default:"1"`
	TransactionDir       string `short:"t" long:"transaction-dir" description:"Path to the transaction directory" default:"/tmp/haproxy"`
	BackupsNumber        int    `short:"n" long:"backups-number" description:"Number of backup configuration files you want to keep, stored in the config dir with version number suffix" default:"0"`
	MasterRuntime        string `short:"m" long:"master-runtime" description:"Path to the master Runtime API socket"`
	ShowSystemInfo       bool   `short:"i" long:"show-system-info" description:"Show system info on info endpoint"`
	DataplaneConfig      string `short:"f" description:"Path to the dataplane configuration file" default:"" yaml:"-"`
	UserListFile         string `long:"userlist-file" description:"Path to the dataplaneapi userlist file. By default userlist is read from HAProxy conf. When specified userlist would be read from this file"`
	NodeIDFile           string `long:"fid" description:"Path to file that will dataplaneapi use to write its id (not a pid) that was given to him after joining a cluster"`
	MapsDir              string `short:"p" long:"maps-dir" description:"Path to maps directory. If set, it reads from specified dir, otherwise it reads from config file"`
	UpdateMapFiles       bool   `long:"update-map-files" description:"Flag used for syncing map files with runtime maps values"`
	UpdateMapFilesPeriod int64  `long:"update-map-files-period" description:"Elapsed time in seconds between two maps syncing operations" default:"10"`
	ClusterTLSCertDir    string `long:"cluster-tls-dir" description:"Path where cluster tls certificates will be stored. Defaults to same directory as dataplane configuration file"`
	MasterWorkerMode     bool   `long:"master-worker-mode" description:"Flag to enable helpers when running within HAProxy"`
}

type APIConfiguration struct {
	APIAddress string `long:"api-address" description:"Advertised API address"`
	APIPort    int64  `long:"api-port" description:"Advertised API port"`
}

type LoggingOptions struct {
	LogTo     string `long:"log-to" description:"Log target, can be stdout or file" default:"stdout" choice:"stdout" choice:"file"`
	LogFile   string `long:"log-file" description:"Location of the log file" default:"/var/log/dataplaneapi/dataplaneapi.log"`
	LogLevel  string `long:"log-level" description:"Logging level" default:"warning" choice:"trace" choice:"debug" choice:"info" choice:"warning" choice:"error"`
	LogFormat string `long:"log-format" description:"Logging format" default:"text" choice:"text" choice:"JSON"`
}

type ClusterConfiguration struct {
	ID                 AtomicString `yaml:"id"`
	ActiveBootstrapKey AtomicString `yaml:"active_bootstrap_key"`
	Token              AtomicString `yaml:"token"`
	URL                AtomicString `yaml:"url"`
	Port               AtomicString `yaml:"port"`
	APIBasePath        AtomicString `yaml:"api_base_path"`
	APINodesPath       AtomicString `yaml:"api_nodes_path"`
	Certificate        ClusterTLS   `yaml:"certificates"`
	Name               AtomicString `yaml:"name"`
	Description        AtomicString `yaml:"description"`
}
type ClusterTLS struct {
	Dir     AtomicString `yaml:"path"`
	Fetched AtomicBool   `yaml:"fetched"`
}

func (c *ClusterConfiguration) Clear() {
	c.ID.Store("")
	c.ActiveBootstrapKey.Store("")
	c.Token.Store("")
	c.Port.Store("")
	c.APIBasePath.Store("")
	c.APINodesPath.Store("")
	c.Certificate.Fetched.Store(false)
	c.Name.Store("")
	c.Description.Store("")
}

type ServerConfiguration struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	APIBasePath string `yaml:"api_base_path"`
}

type NotifyConfiguration struct {
	BootstrapKeyChanged *ChanNotify `yaml:"-"`
	CertificateRefresh  *ChanNotify `yaml:"-"`
	Reload              *ChanNotify `yaml:"-"`
	Shutdown            *ChanNotify `yaml:"-"`
}
type ServiceDiscovery struct {
	mu      sync.Mutex
	Consuls []*models.Consul `yaml:"consuls"`
}

type Configuration struct {
	HAProxy          HAProxyConfiguration `yaml:"-"`
	Logging          LoggingOptions       `yaml:"-"`
	APIOptions       APIConfiguration     `yaml:"-"`
	Cluster          ClusterConfiguration `yaml:"cluster"`
	Server           ServerConfiguration  `yaml:"-"`
	Notify           NotifyConfiguration  `yaml:"-"`
	ServiceDiscovery ServiceDiscovery     `yaml:"service_discovery"`
	Name             AtomicString         `yaml:"name"`
	BootstrapKey     AtomicString         `yaml:"bootstrap_key"`
	Mode             AtomicString         `yaml:"mode" default:"single"`
	Status           AtomicString         `yaml:"status"`
	Cmdline          AtomicString         `yaml:"-"`
}

//Get returns pointer to configuration
func Get() *Configuration {
	if cfg == nil {
		cfg = &Configuration{}
		cfg.initSignalHandler()
		cfg.Notify.BootstrapKeyChanged = NewChanNotify()
		cfg.Notify.CertificateRefresh = NewChanNotify()
		cfg.Notify.Reload = NewChanNotify()
		cfg.Notify.Shutdown = NewChanNotify()

		var sb strings.Builder
		for _, v := range os.Args {
			if !strings.HasPrefix(v, "-") && !strings.Contains(v, `\ `) && strings.ContainsAny(v, " ") {
				fmt.Fprintf(&sb, "\"%s\" ", v)
			} else {
				fmt.Fprintf(&sb, "%s ", v)
			}
		}

		cfg.Cmdline.Store(sb.String())
	}
	return cfg
}

func (c *Configuration) BotstrapKeyChanged(bootstrapKey string) {
	c.BootstrapKey.Store(bootstrapKey)
	err := c.Save()
	if err != nil {
		log.Println(err)
	}
	c.Notify.BootstrapKeyChanged.Notify()
}

func (c *Configuration) UnSubscribeAll() {
	c.Notify.BootstrapKeyChanged.UnSubscribeAll()
	c.Notify.CertificateRefresh.UnSubscribeAll()
	c.Notify.Reload.UnSubscribeAll()
	c.Notify.Shutdown.UnSubscribeAll()
}

func (c *Configuration) Load(swaggerJSON json.RawMessage, host string, port int) error {
	var m map[string]interface{}
	err := json.Unmarshal(swaggerJSON, &m)
	if err != nil {
		return err
	}
	cfg.Server.APIBasePath = m["basePath"].(string)
	if host == "localhost" {
		host = "127.0.0.1"
	}
	cfg.Server.Host = host
	cfg.Server.Port = port

	cfgLoaded := &Configuration{}
	if c.HAProxy.DataplaneConfig != "" {
		yamlFile, err := ioutil.ReadFile(c.HAProxy.DataplaneConfig)
		if err == nil {
			err = yaml.Unmarshal(yamlFile, cfgLoaded)
			if err != nil {
				log.Fatalf("Unmarshal: %v", err)
			}
		}
	}
	c.Cluster = cfgLoaded.Cluster
	c.BootstrapKey.Store(cfgLoaded.BootstrapKey.Load())
	c.Name.Store(cfgLoaded.Name.Load())
	c.Mode.Store(cfgLoaded.Mode.Load())
	c.Status.Store(cfgLoaded.Status.Load())
	c.ServiceDiscovery.Consuls = cfgLoaded.ServiceDiscovery.Consuls

	if c.Mode.Load() == "" {
		c.Mode.Store("single")
	}

	if c.Name.Load() == "" {
		rand.Seed(time.Now().UnixNano())
		c.Name.Store(petname.Generate(2, "_"))
	}

	return nil
}

func (c *Configuration) Save() error {
	if c.HAProxy.DataplaneConfig == "" {
		return nil
	}

	data, err := yaml.Marshal(&c)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	err = renameio.WriteFile(c.HAProxy.DataplaneConfig, data, 0644)
	if err != nil {
		return err
	}
	return nil
}

func (c *Configuration) GetClusterCertDir() string {
	dir := c.Cluster.Certificate.Dir.Load()
	if dir == "" {
		dir = c.HAProxy.ClusterTLSCertDir
	}
	if dir == "" {
		// use same dir as dataplane config file
		url := c.HAProxy.DataplaneConfig
		dir = filepath.Dir(url)
	}
	return dir
}

func (c *Configuration) SaveConsuls(consuls []*models.Consul) error {
	c.ServiceDiscovery.mu.Lock()
	c.ServiceDiscovery.Consuls = consuls
	c.ServiceDiscovery.mu.Unlock()
	return c.Save()
}
