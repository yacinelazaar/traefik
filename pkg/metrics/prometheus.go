package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/containous/traefik/v2/pkg/config/dynamic"
	"github.com/containous/traefik/v2/pkg/log"
	"github.com/containous/traefik/v2/pkg/safe"
	"github.com/containous/traefik/v2/pkg/types"
	"github.com/go-kit/kit/metrics"
	"github.com/gorilla/mux"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// MetricNamePrefix prefix of all metric names
	MetricNamePrefix = "traefik_"

	// server meta information
	metricConfigPrefix             = MetricNamePrefix + "config_"
	configReloadsTotalName         = metricConfigPrefix + "reloads_total"
	configReloadsFailuresTotalName = metricConfigPrefix + "reloads_failure_total"
	configLastReloadSuccessName    = metricConfigPrefix + "last_reload_success"
	configLastReloadFailureName    = metricConfigPrefix + "last_reload_failure"

	// entry point
	metricEntryPointPrefix    = MetricNamePrefix + "entrypoint_"
	entryPointReqsTotalName   = metricEntryPointPrefix + "requests_total"
	entryPointReqDurationName = metricEntryPointPrefix + "request_duration_seconds"
	entryPointOpenConnsName   = metricEntryPointPrefix + "open_connections"

	// service level.

	// MetricServicePrefix prefix of all service metric names
	MetricServicePrefix     = MetricNamePrefix + "service_"
	serviceReqsTotalName    = MetricServicePrefix + "requests_total"
	serviceReqDurationName  = MetricServicePrefix + "request_duration_seconds"
	serviceOpenConnsName    = MetricServicePrefix + "open_connections"
	serviceRetriesTotalName = MetricServicePrefix + "retries_total"
	serviceServerUpName     = MetricServicePrefix + "server_up"
)

// promState holds all metric state internally and acts as the only Collector we register for Prometheus.
//
// This enables control to remove metrics that belong to outdated configuration.
// As an example why this is required, consider Traefik learns about a new service.
// It populates the 'traefik_server_service_up' metric for it with a value of 1 (alive).
// When the service is undeployed now the metric is still there in the client library
// and will be returned on the metrics endpoint until Traefik would be restarted.
//
// To solve this problem promState keeps track of Traefik's dynamic configuration.
// Metrics that "belong" to a dynamic configuration part like services or entryPoints
// are removed after they were scraped at least once when the corresponding object
// doesn't exist anymore.
var promState = newPrometheusState()

var promRegistry = stdprometheus.NewRegistry()

// PrometheusHandler exposes Prometheus routes.
type PrometheusHandler struct{}

// Append adds Prometheus routes on a router.
func (h PrometheusHandler) Append(router *mux.Router) {
	router.Methods(http.MethodGet).Path("/metrics").Handler(promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{}))
}

// RegisterPrometheus registers all Prometheus metrics.
// It must be called only once and failing to register the metrics will lead to a panic.
func RegisterPrometheus(ctx context.Context, config *types.Prometheus) Registry {
	standardRegistry := initStandardRegistry(config)

	if err := promRegistry.Register(stdprometheus.NewProcessCollector(stdprometheus.ProcessCollectorOpts{})); err != nil {
		if _, ok := err.(stdprometheus.AlreadyRegisteredError); !ok {
			log.FromContext(ctx).Warn("ProcessCollector is already registered")
		}
	}
	if err := promRegistry.Register(stdprometheus.NewGoCollector()); err != nil {
		if _, ok := err.(stdprometheus.AlreadyRegisteredError); !ok {
			log.FromContext(ctx).Warn("GoCollector is already registered")
		}
	}

	if !registerPromState(ctx) {
		return nil
	}

	return standardRegistry
}

func initStandardRegistry(config *types.Prometheus) Registry {
	buckets := []float64{0.1, 0.3, 1.2, 5.0}
	if config.Buckets != nil {
		buckets = config.Buckets
	}

	safe.Go(func() {
		promState.ListenValueUpdates()
	})

	configReloads := newCounterFrom(promState.collectors, stdprometheus.CounterOpts{
		Name: configReloadsTotalName,
		Help: "Config reloads",
	}, []string{})
	configReloadsFailures := newCounterFrom(promState.collectors, stdprometheus.CounterOpts{
		Name: configReloadsFailuresTotalName,
		Help: "Config failure reloads",
	}, []string{})
	lastConfigReloadSuccess := newGaugeFrom(promState.collectors, stdprometheus.GaugeOpts{
		Name: configLastReloadSuccessName,
		Help: "Last config reload success",
	}, []string{})
	lastConfigReloadFailure := newGaugeFrom(promState.collectors, stdprometheus.GaugeOpts{
		Name: configLastReloadFailureName,
		Help: "Last config reload failure",
	}, []string{})

	promState.describers = []func(chan<- *stdprometheus.Desc){
		configReloads.cv.Describe,
		configReloadsFailures.cv.Describe,
		lastConfigReloadSuccess.gv.Describe,
		lastConfigReloadFailure.gv.Describe,
	}

	reg := &standardRegistry{
		epEnabled:                    config.AddEntryPointsLabels,
		svcEnabled:                   config.AddServicesLabels,
		configReloadsCounter:         configReloads,
		configReloadsFailureCounter:  configReloadsFailures,
		lastConfigReloadSuccessGauge: lastConfigReloadSuccess,
		lastConfigReloadFailureGauge: lastConfigReloadFailure,
	}

	if config.AddEntryPointsLabels {
		entryPointReqs := newCounterFrom(promState.collectors, stdprometheus.CounterOpts{
			Name: entryPointReqsTotalName,
			Help: "How many HTTP requests processed on an entrypoint, partitioned by status code, protocol, and method.",
		}, []string{"code", "method", "protocol", "entrypoint"})
		entryPointReqDurations := newHistogramFrom(promState.collectors, stdprometheus.HistogramOpts{
			Name:    entryPointReqDurationName,
			Help:    "How long it took to process the request on an entrypoint, partitioned by status code, protocol, and method.",
			Buckets: buckets,
		}, []string{"code", "method", "protocol", "entrypoint"})
		entryPointOpenConns := newGaugeFrom(promState.collectors, stdprometheus.GaugeOpts{
			Name: entryPointOpenConnsName,
			Help: "How many open connections exist on an entrypoint, partitioned by method and protocol.",
		}, []string{"method", "protocol", "entrypoint"})

		promState.describers = append(promState.describers, []func(chan<- *stdprometheus.Desc){
			entryPointReqs.cv.Describe,
			entryPointReqDurations.hv.Describe,
			entryPointOpenConns.gv.Describe,
		}...)
		reg.entryPointReqsCounter = entryPointReqs
		reg.entryPointReqDurationHistogram = entryPointReqDurations
		reg.entryPointOpenConnsGauge = entryPointOpenConns
	}
	if config.AddServicesLabels {
		serviceReqs := newCounterFrom(promState.collectors, stdprometheus.CounterOpts{
			Name: serviceReqsTotalName,
			Help: "How many HTTP requests processed on a service, partitioned by status code, protocol, and method.",
		}, []string{"code", "method", "protocol", "service"})
		serviceReqDurations := newHistogramFrom(promState.collectors, stdprometheus.HistogramOpts{
			Name:    serviceReqDurationName,
			Help:    "How long it took to process the request on a service, partitioned by status code, protocol, and method.",
			Buckets: buckets,
		}, []string{"code", "method", "protocol", "service"})
		serviceOpenConns := newGaugeFrom(promState.collectors, stdprometheus.GaugeOpts{
			Name: serviceOpenConnsName,
			Help: "How many open connections exist on a service, partitioned by method and protocol.",
		}, []string{"method", "protocol", "service"})
		serviceRetries := newCounterFrom(promState.collectors, stdprometheus.CounterOpts{
			Name: serviceRetriesTotalName,
			Help: "How many request retries happened on a service.",
		}, []string{"service"})
		serviceServerUp := newGaugeFrom(promState.collectors, stdprometheus.GaugeOpts{
			Name: serviceServerUpName,
			Help: "service server is up, described by gauge value of 0 or 1.",
		}, []string{"service", "url"})

		promState.describers = append(promState.describers, []func(chan<- *stdprometheus.Desc){
			serviceReqs.cv.Describe,
			serviceReqDurations.hv.Describe,
			serviceOpenConns.gv.Describe,
			serviceRetries.cv.Describe,
			serviceServerUp.gv.Describe,
		}...)

		reg.serviceReqsCounter = serviceReqs
		reg.serviceReqDurationHistogram = serviceReqDurations
		reg.serviceOpenConnsGauge = serviceOpenConns
		reg.serviceRetriesCounter = serviceRetries
		reg.serviceServerUpGauge = serviceServerUp
	}

	return reg
}

func registerPromState(ctx context.Context) bool {
	if err := promRegistry.Register(promState); err != nil {
		logger := log.FromContext(ctx)
		if _, ok := err.(stdprometheus.AlreadyRegisteredError); !ok {
			logger.Errorf("Unable to register Traefik to Prometheus: %v", err)
			return false
		}
		logger.Debug("Prometheus collector already registered.")
	}
	return true
}

// OnConfigurationUpdate receives the current configuration from Traefik.
// It then converts the configuration to the optimized package internal format
// and sets it to the promState.
func OnConfigurationUpdate(dynConf dynamic.Configurations, entryPoints []string) {
	dynamicConfig := newDynamicConfig()

	for _, value := range entryPoints {
		dynamicConfig.entryPoints[value] = true
	}
	for key, config := range dynConf {
		for name := range config.HTTP.Routers {
			dynamicConfig.routers[fmt.Sprintf("%s@%s", name, key)] = true
		}

		for serviceName, service := range config.HTTP.Services {
			dynamicConfig.services[fmt.Sprintf("%s@%s", serviceName, key)] = make(map[string]bool)
			if service.LoadBalancer != nil {
				for _, server := range service.LoadBalancer.Servers {
					dynamicConfig.services[fmt.Sprintf("%s@%s", serviceName, key)][server.URL] = true
				}
			}
		}
	}

	promState.SetDynamicConfig(dynamicConfig)
}

func newPrometheusState() *prometheusState {
	return &prometheusState{
		collectors:    make(chan *collector),
		dynamicConfig: newDynamicConfig(),
		state:         make(map[string]*collector),
	}
}

type prometheusState struct {
	collectors chan *collector
	describers []func(ch chan<- *stdprometheus.Desc)

	mtx           sync.Mutex
	dynamicConfig *dynamicConfig
	state         map[string]*collector
}

func (ps *prometheusState) SetDynamicConfig(dynamicConfig *dynamicConfig) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()
	ps.dynamicConfig = dynamicConfig
}

func (ps *prometheusState) ListenValueUpdates() {
	for collector := range ps.collectors {
		ps.mtx.Lock()
		ps.state[collector.id] = collector
		ps.mtx.Unlock()
	}
}

// Describe implements prometheus.Collector and simply calls
// the registered describer functions.
func (ps *prometheusState) Describe(ch chan<- *stdprometheus.Desc) {
	for _, desc := range ps.describers {
		desc(ch)
	}
}

// Collect implements prometheus.Collector. It calls the Collect
// method of all metrics it received on the collectors channel.
// It's also responsible to remove metrics that belong to an outdated configuration.
// The removal happens only after their Collect method was called to ensure that
// also those metrics will be exported on the current scrape.
func (ps *prometheusState) Collect(ch chan<- stdprometheus.Metric) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	var outdatedKeys []string
	for key, cs := range ps.state {
		cs.collector.Collect(ch)

		if ps.isOutdated(cs) {
			outdatedKeys = append(outdatedKeys, key)
		}
	}

	for _, key := range outdatedKeys {
		ps.state[key].delete()
		delete(ps.state, key)
	}
}

// isOutdated checks whether the passed collector has labels that mark
// it as belonging to an outdated configuration of Traefik.
func (ps *prometheusState) isOutdated(collector *collector) bool {
	labels := collector.labels

	if entrypointName, ok := labels["entrypoint"]; ok && !ps.dynamicConfig.hasEntryPoint(entrypointName) {
		return true
	}

	if serviceName, ok := labels["service"]; ok {
		if !ps.dynamicConfig.hasService(serviceName) {
			return true
		}
		if url, ok := labels["url"]; ok && !ps.dynamicConfig.hasServerURL(serviceName, url) {
			return true
		}
	}

	return false
}

func newDynamicConfig() *dynamicConfig {
	return &dynamicConfig{
		entryPoints: make(map[string]bool),
		routers:     make(map[string]bool),
		services:    make(map[string]map[string]bool),
	}
}

// dynamicConfig holds the current configuration for entryPoints, services,
// and server URLs in an optimized way to check for existence. This provides
// a performant way to check whether the collected metrics belong to the
// current configuration or to an outdated one.
type dynamicConfig struct {
	entryPoints map[string]bool
	routers     map[string]bool
	services    map[string]map[string]bool
}

func (d *dynamicConfig) hasEntryPoint(entrypointName string) bool {
	_, ok := d.entryPoints[entrypointName]
	return ok
}

func (d *dynamicConfig) hasService(serviceName string) bool {
	_, ok := d.services[serviceName]
	return ok
}

func (d *dynamicConfig) hasServerURL(serviceName, serverURL string) bool {
	if service, hasService := d.services[serviceName]; hasService {
		_, ok := service[serverURL]
		return ok
	}
	return false
}

func newCollector(metricName string, labels stdprometheus.Labels, c stdprometheus.Collector, delete func()) *collector {
	return &collector{
		id:        buildMetricID(metricName, labels),
		labels:    labels,
		collector: c,
		delete:    delete,
	}
}

// collector wraps a Collector object from the Prometheus client library.
// It adds information on how many generations this metric should be present
// in the /metrics output, relatived to the time it was last tracked.
type collector struct {
	id        string
	labels    stdprometheus.Labels
	collector stdprometheus.Collector
	delete    func()
}

func buildMetricID(metricName string, labels stdprometheus.Labels) string {
	var labelNamesValues []string
	for name, value := range labels {
		labelNamesValues = append(labelNamesValues, name, value)
	}
	sort.Strings(labelNamesValues)
	return metricName + ":" + strings.Join(labelNamesValues, "|")
}

func newCounterFrom(collectors chan<- *collector, opts stdprometheus.CounterOpts, labelNames []string) *counter {
	cv := stdprometheus.NewCounterVec(opts, labelNames)
	c := &counter{
		name:       opts.Name,
		cv:         cv,
		collectors: collectors,
	}
	if len(labelNames) == 0 {
		c.Add(0)
	}
	return c
}

type counter struct {
	name             string
	cv               *stdprometheus.CounterVec
	labelNamesValues labelNamesValues
	collectors       chan<- *collector
}

func (c *counter) With(labelValues ...string) metrics.Counter {
	return &counter{
		name:             c.name,
		cv:               c.cv,
		labelNamesValues: c.labelNamesValues.With(labelValues...),
		collectors:       c.collectors,
	}
}

func (c *counter) Add(delta float64) {
	labels := c.labelNamesValues.ToLabels()
	collector := c.cv.With(labels)
	collector.Add(delta)
	c.collectors <- newCollector(c.name, labels, collector, func() {
		c.cv.Delete(labels)
	})
}

func (c *counter) Describe(ch chan<- *stdprometheus.Desc) {
	c.cv.Describe(ch)
}

func newGaugeFrom(collectors chan<- *collector, opts stdprometheus.GaugeOpts, labelNames []string) *gauge {
	gv := stdprometheus.NewGaugeVec(opts, labelNames)
	g := &gauge{
		name:       opts.Name,
		gv:         gv,
		collectors: collectors,
	}
	if len(labelNames) == 0 {
		g.Set(0)
	}
	return g
}

type gauge struct {
	name             string
	gv               *stdprometheus.GaugeVec
	labelNamesValues labelNamesValues
	collectors       chan<- *collector
}

func (g *gauge) With(labelValues ...string) metrics.Gauge {
	return &gauge{
		name:             g.name,
		gv:               g.gv,
		labelNamesValues: g.labelNamesValues.With(labelValues...),
		collectors:       g.collectors,
	}
}

func (g *gauge) Add(delta float64) {
	labels := g.labelNamesValues.ToLabels()
	collector := g.gv.With(labels)
	collector.Add(delta)
	g.collectors <- newCollector(g.name, labels, collector, func() {
		g.gv.Delete(labels)
	})
}

func (g *gauge) Set(value float64) {
	labels := g.labelNamesValues.ToLabels()
	collector := g.gv.With(labels)
	collector.Set(value)
	g.collectors <- newCollector(g.name, labels, collector, func() {
		g.gv.Delete(labels)
	})
}

func (g *gauge) Describe(ch chan<- *stdprometheus.Desc) {
	g.gv.Describe(ch)
}

func newHistogramFrom(collectors chan<- *collector, opts stdprometheus.HistogramOpts, labelNames []string) *histogram {
	hv := stdprometheus.NewHistogramVec(opts, labelNames)
	return &histogram{
		name:       opts.Name,
		hv:         hv,
		collectors: collectors,
	}
}

type histogram struct {
	name             string
	hv               *stdprometheus.HistogramVec
	labelNamesValues labelNamesValues
	collectors       chan<- *collector
}

func (h *histogram) With(labelValues ...string) metrics.Histogram {
	return &histogram{
		name:             h.name,
		hv:               h.hv,
		labelNamesValues: h.labelNamesValues.With(labelValues...),
		collectors:       h.collectors,
	}
}

func (h *histogram) Observe(value float64) {
	labels := h.labelNamesValues.ToLabels()
	observer := h.hv.With(labels)
	observer.Observe(value)
	// Do a type assertion to be sure that prometheus will be able to call the Collect method.
	if collector, ok := observer.(stdprometheus.Histogram); ok {
		h.collectors <- newCollector(h.name, labels, collector, func() {
			h.hv.Delete(labels)
		})
	}
}

func (h *histogram) Describe(ch chan<- *stdprometheus.Desc) {
	h.hv.Describe(ch)
}

// labelNamesValues is a type alias that provides validation on its With method.
// Metrics may include it as a member to help them satisfy With semantics and
// save some code duplication.
type labelNamesValues []string

// With validates the input, and returns a new aggregate labelNamesValues.
func (lvs labelNamesValues) With(labelValues ...string) labelNamesValues {
	if len(labelValues)%2 != 0 {
		labelValues = append(labelValues, "unknown")
	}
	return append(lvs, labelValues...)
}

// ToLabels is a convenience method to convert a labelNamesValues
// to the native prometheus.Labels.
func (lvs labelNamesValues) ToLabels() stdprometheus.Labels {
	labels := stdprometheus.Labels{}
	for i := 0; i < len(lvs); i += 2 {
		labels[lvs[i]] = lvs[i+1]
	}
	return labels
}
