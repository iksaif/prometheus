// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package consul

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	consul "github.com/hashicorp/consul/api"
	"github.com/mwitkow/go-conntrack"
	"github.com/prometheus/client_golang/prometheus"
	config_util "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/util/httputil"
	"github.com/prometheus/prometheus/util/strutil"
	yaml_util "github.com/prometheus/prometheus/util/yaml"
)

const (
	watchTimeout  = 30 * time.Second
	retryInterval = 15 * time.Second

	// addressLabel is the name for the label containing a target's address.
	addressLabel = model.MetaLabelPrefix + "consul_address"
	// nodeLabel is the name for the label containing a target's node name.
	nodeLabel = model.MetaLabelPrefix + "consul_node"
	// metaDataLabel is the prefix for the labels mapping to a target's metadata.
	metaDataLabel = model.MetaLabelPrefix + "consul_metadata_"
	// tagsLabel is the name of the label containing the tags assigned to the target.
	tagsLabel = model.MetaLabelPrefix + "consul_tags"
	// serviceLabel is the name of the label containing the service name.
	serviceLabel = model.MetaLabelPrefix + "consul_service"
	// serviceAddressLabel is the name of the label containing the (optional) service address.
	serviceAddressLabel = model.MetaLabelPrefix + "consul_service_address"
	//servicePortLabel is the name of the label containing the service port.
	servicePortLabel = model.MetaLabelPrefix + "consul_service_port"
	// datacenterLabel is the name of the label containing the datacenter ID.
	datacenterLabel = model.MetaLabelPrefix + "consul_dc"
	// serviceIDLabel is the name of the label containing the service ID.
	serviceIDLabel = model.MetaLabelPrefix + "consul_service_id"

	// Constants for instrumentation.
	namespace = "prometheus"
)

var (
	rpcFailuresCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "sd_consul_rpc_failures_total",
			Help:      "The number of Consul RPC call failures.",
		})
	rpcDuration = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace: namespace,
			Name:      "sd_consul_rpc_duration_seconds",
			Help:      "The duration of a Consul RPC call in seconds.",
		},
		[]string{"endpoint", "call"},
	)

	// DefaultSDConfig is the default Consul SD configuration.
	DefaultSDConfig = SDConfig{
		TagSeparator: ",",
		Scheme:       "http",
		AllowStale:   true,
		RefreshInterval: model.Duration(0 * time.Second),
	}
)

// SDConfig is the configuration for Consul service discovery.
type SDConfig struct {
	Server       string             `yaml:"server"`
	Token        config_util.Secret `yaml:"token,omitempty"`
	Datacenter   string             `yaml:"datacenter,omitempty"`
	TagSeparator string             `yaml:"tag_separator,omitempty"`
	Scheme       string             `yaml:"scheme,omitempty"`
	Username     string             `yaml:"username,omitempty"`
	Password     config_util.Secret `yaml:"password,omitempty"`
	// See https://www.consul.io/docs/internals/consensus.html#consistency-modes,
	// stale reads are a lot cheaper and are a necessity if you have >5k targets.
	AllowStale   bool               `yaml:"allow_stale,omitempty"`
	// By default use blocking queries () but allow users to delay
	// updates if necessary. This can be useful because of "bugs" like
	// https://github.com/hashicorp/consul/issues/3712 which cause an un-necessary
	// amount of requests on consul.
	RefreshInterval model.Duration  `yaml:"refresh_interval,omitempty"`

	// The list of services for which targets are discovered.
	// Defaults to all services if empty.
	Services []string `yaml:"services"`
	// An optional tag used to filter instances inside a service.
	Tag string `yaml:"tag"`

	TLSConfig config_util.TLSConfig `yaml:"tls_config,omitempty"`
	// Catches all undefined fields and must be empty after parsing.
	XXX map[string]interface{} `yaml:",inline"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *SDConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = DefaultSDConfig
	type plain SDConfig
	err := unmarshal((*plain)(c))
	if err != nil {
		return err
	}
	if err := yaml_util.CheckOverflow(c.XXX, "consul_sd_config"); err != nil {
		return err
	}
	if strings.TrimSpace(c.Server) == "" {
		return fmt.Errorf("Consul SD configuration requires a server address")
	}
	return nil
}

func init() {
	prometheus.MustRegister(rpcFailuresCount)
	prometheus.MustRegister(rpcDuration)

	// Initialize metric vectors.
	rpcDuration.WithLabelValues("catalog", "service")
	rpcDuration.WithLabelValues("catalog", "services")
}

// Discovery retrieves target information from a Consul server
// and updates them via watches.
type Discovery struct {
	client           *consul.Client
	clientConf       *consul.Config
	clientDatacenter string
	tagSeparator     string
	watchedServices  []string // Set of services which will be discovered.
	watchedTag       string   // A tag used to filter instances of a service.
	allowStale       bool
	refreshInterval  time.Duration
	logger           log.Logger
}

// NewDiscovery returns a new Discovery for the given config.
func NewDiscovery(conf *SDConfig, logger log.Logger) (*Discovery, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	tls, err := httputil.NewTLSConfig(conf.TLSConfig)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		// FIXME: test
		//MaxIdleConns:        20000,
		//MaxIdleConnsPerHost: 1000, // see https://github.com/golang/go/issues/13801
		//DisableKeepAlives:   false,
		TLSClientConfig: tls,
		DialContext: conntrack.NewDialContextFunc(
			conntrack.DialWithTracing(),
			conntrack.DialWithName("consul_sd"),
		),
	}
	wrapper := &http.Client{
		Transport: transport,
		Timeout:   35 * time.Second,
	}

	clientConf := &consul.Config{
		Address:    conf.Server,
		Scheme:     conf.Scheme,
		Datacenter: conf.Datacenter,
		Token:      string(conf.Token),
		HttpAuth: &consul.HttpBasicAuth{
			Username: conf.Username,
			Password: string(conf.Password),
		},
		HttpClient: wrapper,
	}
	client, err := consul.NewClient(clientConf)
	if err != nil {
		return nil, err
	}
	cd := &Discovery{
		client:           client,
		clientConf:       clientConf,
		tagSeparator:     conf.TagSeparator,
		watchedServices:  conf.Services,
		watchedTag:       conf.Tag,
		allowStale:       conf.AllowStale,
		refreshInterval:  time.Duration(conf.RefreshInterval),
		clientDatacenter: clientConf.Datacenter,
		logger:           logger,
	}
	return cd, nil
}

// shouldWatch returns whether the service of the given name should be watched.
func (d *Discovery) shouldWatch(name string, tags []string) bool {
	return d.shouldWatchFromName(name) && d.shouldWatchFromTags(tags)
}

// shouldWatch returns whether the service of the given name should be watched based on its name.
func (d *Discovery) shouldWatchFromName(name string) bool {
	// If there's no fixed set of watched services, we watch everything.
	if len(d.watchedServices) == 0 {
		return true
	}

	for _, sn := range d.watchedServices {
		if sn == name {
			return true
		}
	}
	return false
}

// shouldWatch returns whether the service of the given name should be watched based on its tags.
func (d *Discovery) shouldWatchFromTags(tags []string) bool {
	// If there's no fixed set of watched tags, we watch everything.
	if d.watchedTag == "" {
		return true
	}

	for _, tag := range tags {
		if d.watchedTag == tag {
			return true
		}
	}
	return false
}

// Get the local datacenter if not specified.
func (d *Discovery) getDatacenter() error {
	// If the datacenter was not set from clientConf, let's get it from the local Consul agent
	// (Consul default is to use local node's datacenter if one isn't given for a query).
	if d.clientDatacenter != "" {
		return nil
	}

	info, err := d.client.Agent().Self()
	if err != nil {
		level.Error(d.logger).Log("msg", "Error retrieving datacenter name", "err", err)
		return err
	}

	d.clientDatacenter = info["Config"]["Datacenter"].(string)
	return nil
}

// Run implements the Discoverer interface.
func (d *Discovery) Run(ctx context.Context, ch chan<- []*targetgroup.Group) {
	// Watched services and their cancellation functions.
	services := map[string]func(){}

	var lastIndex uint64
	for {
		// We have to check the context at least once. The checks during channel sends
		// do not guarantee that.
		select {
		case <-ctx.Done():
			break
		default:
		}

		// Get the local datacenter first, if necessary.
		err := d.getDatacenter()
		if err != nil {
			time.Sleep(retryInterval)
			continue
		}

		if len(d.watchedServices) == 0 || d.watchedTag != "" {
			// We need to watch the catalog.
			d.watchServices(ctx, ch, &lastIndex, services)
		} else {
			// We only have fully defined services.
			for _, name := range d.watchedServices {
				ctx, _ := context.WithCancel(ctx)
				d.watchService(name, ctx, ch)
			}
			// Wait for cancellation.
			<-ctx.Done()
			break
		}
	}
}

// Watch the catalog for services we would like to watch. This is only necessary
// for services filtered by tag. If we already have the name it is more efficient to
// watch nodes the service directly.
func (d *Discovery) watchServices(ctx context.Context, ch chan<- []*targetgroup.Group, lastIndex *uint64, services map[string]func()) error {
	catalog := d.client.Catalog()
	level.Debug(d.logger).Log("msg", "Watching services", "tag", d.watchedTag)

	t0 := time.Now()
	srvs, meta, err := catalog.Services(&consul.QueryOptions{
		WaitIndex:  *lastIndex,
		WaitTime:   watchTimeout,
		AllowStale: d.allowStale,
	})
	rpcDuration.WithLabelValues("catalog", "services").Observe(time.Since(t0).Seconds())

	if err != nil {
		level.Error(d.logger).Log("msg", "Error refreshing service list", "err", err)
		rpcFailuresCount.Inc()
		time.Sleep(retryInterval)
		return err
	}
	// If the index equals the previous one, the watch timed out with no update.
	if meta.LastIndex == *lastIndex {
		return nil
	}
	*lastIndex = meta.LastIndex

	// Check for new services.
	for name := range srvs {
		// catalog.Service() returns a map of service name to tags, we can use that to watch
		// only the services that have the tag we are looking for (if specified).
		// In the future consul may support server side filtering for both names and tags:
		// https://github.com/hashicorp/consul/pull/2549#issuecomment-293500452
		if !d.shouldWatch(name, srvs[name]) {
			continue
		}
		if _, ok := services[name]; ok {
			continue // We are already watching the service.
		}

		wctx, cancel := context.WithCancel(ctx)
		d.watchService(name, wctx, ch)
		services[name] = cancel
	}

	// Check for removed services.
	for name, cancel := range services {
		if _, ok := srvs[name]; !ok {
			// Call the watch cancellation function.
			cancel()
			delete(services, name)

			// Send clearing target group.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- []*targetgroup.Group{{Source: name}}:
			}
		}
	}

	time.Sleep(d.refreshInterval)
	return nil
}

// consulService contains data belonging to the same service.
type consulService struct {
	name         string
	tag          string
	labels       model.LabelSet
	discovery    *Discovery
	client       *consul.Client
	tagSeparator string
	logger       log.Logger
}

// Start watching a service.
func (d *Discovery) watchService(name string, ctx context.Context, ch chan<- []*targetgroup.Group) {
	srv := &consulService{
		discovery: d,
		client:    d.client,
		name:      name,
		tag:       d.watchedTag,
		labels: model.LabelSet{
			serviceLabel:    model.LabelValue(name),
			datacenterLabel: model.LabelValue(d.clientDatacenter),
		},
		tagSeparator: d.tagSeparator,
		logger:       d.logger,
	}

	go srv.watch(ctx, ch)
}

// Continuously watch one service.
func (srv *consulService) watch(ctx context.Context, ch chan<- []*targetgroup.Group) {
	catalog := srv.client.Catalog()

	lastIndex := uint64(0)
	for {
		level.Debug(srv.logger).Log("msg", "Watching service", "service", srv.name, "tag", srv.tag)

		t0 := time.Now()
		nodes, meta, err := catalog.Service(srv.name, srv.tag, &consul.QueryOptions{
			WaitIndex:  lastIndex,
			WaitTime:   watchTimeout,
			AllowStale: srv.discovery.allowStale,
		})
		rpcDuration.WithLabelValues("catalog", "service").Observe(time.Since(t0).Seconds())

		// Check the context before potentially falling in a continue-loop.
		select {
		case <-ctx.Done():
			return
		default:
			// Continue.
		}

		if err != nil {
			level.Error(srv.logger).Log("msg", "Error refreshing service", "service", srv.name, "tag", srv.tag, "err", err)
			rpcFailuresCount.Inc()
			time.Sleep(retryInterval)
			continue
		}
		// If the index equals the previous one, the watch timed out with no update.
		if meta.LastIndex == lastIndex {
			continue
		}
		lastIndex = meta.LastIndex

		tgroup := targetgroup.Group{
			Source:  srv.name,
			Labels:  srv.labels,
			Targets: make([]model.LabelSet, 0, len(nodes)),
		}

		for _, node := range nodes {

			// We surround the separated list with the separator as well. This way regular expressions
			// in relabeling rules don't have to consider tag positions.
			var tags = srv.tagSeparator + strings.Join(node.ServiceTags, srv.tagSeparator) + srv.tagSeparator

			// If the service address is not empty it should be used instead of the node address
			// since the service may be registered remotely through a different node
			var addr string
			if node.ServiceAddress != "" {
				addr = net.JoinHostPort(node.ServiceAddress, fmt.Sprintf("%d", node.ServicePort))
			} else {
				addr = net.JoinHostPort(node.Address, fmt.Sprintf("%d", node.ServicePort))
			}

			labels := model.LabelSet{
				model.AddressLabel:  model.LabelValue(addr),
				addressLabel:        model.LabelValue(node.Address),
				nodeLabel:           model.LabelValue(node.Node),
				tagsLabel:           model.LabelValue(tags),
				serviceAddressLabel: model.LabelValue(node.ServiceAddress),
				servicePortLabel:    model.LabelValue(strconv.Itoa(node.ServicePort)),
				serviceIDLabel:      model.LabelValue(node.ServiceID),
			}

			// Add all key/value pairs from the node's metadata as their own labels
			for k, v := range node.NodeMeta {
				name := strutil.SanitizeLabelName(k)
				labels[metaDataLabel+model.LabelName(name)] = model.LabelValue(v)
			}

			tgroup.Targets = append(tgroup.Targets, labels)
		}
		// Check context twice to ensure we always catch cancellation.
		select {
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case ch <- []*targetgroup.Group{&tgroup}:
		}
		time.Sleep(srv.discovery.refreshInterval)
	}
}
