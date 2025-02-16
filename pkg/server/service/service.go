package service

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/containous/alice"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/config/dynamic"
	"github.com/traefik/traefik/v3/pkg/config/runtime"
	"github.com/traefik/traefik/v3/pkg/healthcheck"
	"github.com/traefik/traefik/v3/pkg/logs"
	"github.com/traefik/traefik/v3/pkg/middlewares/accesslog"
	"github.com/traefik/traefik/v3/pkg/middlewares/capture"
	metricsMiddle "github.com/traefik/traefik/v3/pkg/middlewares/metrics"
	"github.com/traefik/traefik/v3/pkg/middlewares/observability"
	"github.com/traefik/traefik/v3/pkg/proxy/httputil"
	"github.com/traefik/traefik/v3/pkg/safe"
	"github.com/traefik/traefik/v3/pkg/server/cookie"
	"github.com/traefik/traefik/v3/pkg/server/middleware"
	"github.com/traefik/traefik/v3/pkg/server/provider"
	"github.com/traefik/traefik/v3/pkg/server/service/loadbalancer/failover"
	"github.com/traefik/traefik/v3/pkg/server/service/loadbalancer/hrw"
	"github.com/traefik/traefik/v3/pkg/server/service/loadbalancer/mirror"
	"github.com/traefik/traefik/v3/pkg/server/service/loadbalancer/wrr"
	"google.golang.org/grpc/status"
)

// ProxyBuilder builds reverse proxy handlers.
type ProxyBuilder interface {
	Build(cfgName string, targetURL *url.URL, shouldObserve, passHostHeader, preservePath bool, flushInterval time.Duration) (http.Handler, error)
	Update(configs map[string]*dynamic.ServersTransport)
}

// ServiceBuilder is a Service builder.
type ServiceBuilder interface {
	BuildHTTP(rootCtx context.Context, serviceName string) (http.Handler, error)
}

// Manager The service manager.
type Manager struct {
	routinePool      *safe.Pool
	observabilityMgr *middleware.ObservabilityMgr
	transportManager httputil.TransportManager
	proxyBuilder     ProxyBuilder
	serviceBuilders  []ServiceBuilder

	services       map[string]http.Handler
	configs        map[string]*runtime.ServiceInfo
	healthCheckers map[string]*healthcheck.ServiceHealthChecker
	rand           *rand.Rand // For the initial shuffling of load-balancers.
}

// NewManager creates a new Manager.
func NewManager(configs map[string]*runtime.ServiceInfo, observabilityMgr *middleware.ObservabilityMgr, routinePool *safe.Pool, transportManager httputil.TransportManager, proxyBuilder ProxyBuilder, serviceBuilders ...ServiceBuilder) *Manager {
	return &Manager{
		routinePool:      routinePool,
		observabilityMgr: observabilityMgr,
		transportManager: transportManager,
		proxyBuilder:     proxyBuilder,
		serviceBuilders:  serviceBuilders,
		services:         make(map[string]http.Handler),
		configs:          configs,
		healthCheckers:   make(map[string]*healthcheck.ServiceHealthChecker),
		rand:             rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// BuildHTTP Creates a http.Handler for a service configuration.
func (m *Manager) BuildHTTP(rootCtx context.Context, serviceName string) (http.Handler, error) {
	serviceName = provider.GetQualifiedName(rootCtx, serviceName)

	ctx := log.Ctx(rootCtx).With().Str(logs.ServiceName, serviceName).Logger().
		WithContext(provider.AddInContext(rootCtx, serviceName))

	handler, ok := m.services[serviceName]
	if ok {
		return handler, nil
	}

	// Must be before we get configs to handle services without config.
	for _, builder := range m.serviceBuilders {
		handler, err := builder.BuildHTTP(rootCtx, serviceName)
		if err != nil {
			return nil, err
		}
		if handler != nil {
			m.services[serviceName] = handler
			return handler, nil
		}
	}

	conf, ok := m.configs[serviceName]
	if !ok {
		return nil, fmt.Errorf("the service %q does not exist", serviceName)
	}

	if conf.Status == runtime.StatusDisabled {
		return nil, errors.New(strings.Join(conf.Err, ", "))
	}

	value := reflect.ValueOf(*conf.Service)
	var count int
	for i := range value.NumField() {
		if !value.Field(i).IsNil() {
			count++
		}
	}
	if count > 1 {
		err := errors.New("cannot create service: multi-types service not supported, consider declaring two different pieces of service instead")
		conf.AddError(err, true)
		return nil, err
	}

	var lb http.Handler

	switch {
	case conf.LoadBalancer != nil:
		var err error
		lb, err = m.getLoadBalancerServiceHandler(ctx, serviceName, conf)
		if err != nil {
			conf.AddError(err, true)
			return nil, err
		}
	case conf.Weighted != nil:
		var err error
		lb, err = m.getWRRServiceHandler(ctx, serviceName, conf.Weighted)
		if err != nil {
			conf.AddError(err, true)
			return nil, err
		}
	case conf.HighestRandomWeight != nil:
		var err error
		lb, err = m.getHRWServiceHandler(ctx, serviceName, conf.HighestRandomWeight)
		if err != nil {
			conf.AddError(err, true)
			return nil, err
		}
	case conf.Mirroring != nil:
		var err error
		lb, err = m.getMirrorServiceHandler(ctx, conf.Mirroring)
		if err != nil {
			conf.AddError(err, true)
			return nil, err
		}
	case conf.Failover != nil:
		var err error
		lb, err = m.getFailoverServiceHandler(ctx, serviceName, conf.Failover)
		if err != nil {
			conf.AddError(err, true)
			return nil, err
		}
	default:
		sErr := fmt.Errorf("the service %q does not have any type defined", serviceName)
		conf.AddError(sErr, true)
		return nil, sErr
	}

	m.services[serviceName] = lb

	return lb, nil
}

func (m *Manager) getFailoverServiceHandler(ctx context.Context, serviceName string, config *dynamic.Failover) (http.Handler, error) {
	f := failover.New(config.HealthCheck)

	serviceHandler, err := m.BuildHTTP(ctx, config.Service)
	if err != nil {
		return nil, err
	}

	f.SetHandler(serviceHandler)

	updater, ok := serviceHandler.(healthcheck.StatusUpdater)
	if !ok {
		return nil, fmt.Errorf("child service %v of %v not a healthcheck.StatusUpdater (%T)", config.Service, serviceName, serviceHandler)
	}

	if err := updater.RegisterStatusUpdater(func(up bool) {
		f.SetHandlerStatus(ctx, up)
	}); err != nil {
		return nil, fmt.Errorf("cannot register %v as updater for %v: %w", config.Service, serviceName, err)
	}

	fallbackHandler, err := m.BuildHTTP(ctx, config.Fallback)
	if err != nil {
		return nil, err
	}

	f.SetFallbackHandler(fallbackHandler)

	// Do not report the health of the fallback handler.
	if config.HealthCheck == nil {
		return f, nil
	}

	fallbackUpdater, ok := fallbackHandler.(healthcheck.StatusUpdater)
	if !ok {
		return nil, fmt.Errorf("child service %v of %v not a healthcheck.StatusUpdater (%T)", config.Fallback, serviceName, fallbackHandler)
	}

	if err := fallbackUpdater.RegisterStatusUpdater(func(up bool) {
		f.SetFallbackHandlerStatus(ctx, up)
	}); err != nil {
		return nil, fmt.Errorf("cannot register %v as updater for %v: %w", config.Fallback, serviceName, err)
	}

	return f, nil
}

func (m *Manager) getMirrorServiceHandler(ctx context.Context, config *dynamic.Mirroring) (http.Handler, error) {
	serviceHandler, err := m.BuildHTTP(ctx, config.Service)
	if err != nil {
		return nil, err
	}

	mirrorBody := dynamic.MirroringDefaultMirrorBody
	if config.MirrorBody != nil {
		mirrorBody = *config.MirrorBody
	}

	maxBodySize := dynamic.MirroringDefaultMaxBodySize
	if config.MaxBodySize != nil {
		maxBodySize = *config.MaxBodySize
	}
	handler := mirror.New(serviceHandler, m.routinePool, mirrorBody, maxBodySize, config.HealthCheck)
	for _, mirrorConfig := range config.Mirrors {
		mirrorHandler, err := m.BuildHTTP(ctx, mirrorConfig.Name)
		if err != nil {
			return nil, err
		}

		err = handler.AddMirror(mirrorHandler, mirrorConfig.Percent)
		if err != nil {
			return nil, err
		}
	}
	return handler, nil
}

func (m *Manager) getWRRServiceHandler(ctx context.Context, serviceName string, config *dynamic.WeightedRoundRobin) (http.Handler, error) {
	// TODO Handle accesslog and metrics with multiple service name
	if config.Sticky != nil && config.Sticky.Cookie != nil {
		config.Sticky.Cookie.Name = cookie.GetName(config.Sticky.Cookie.Name, serviceName)
	}

	balancer := wrr.New(config.Sticky, config.HealthCheck != nil)
	for _, service := range shuffle(config.Services, m.rand) {
		serviceHandler, err := m.getServiceHandler(ctx, service)
		if err != nil {
			return nil, err
		}

		balancer.Add(service.Name, serviceHandler, service.Weight, false)

		if config.HealthCheck == nil {
			continue
		}

		childName := service.Name
		updater, ok := serviceHandler.(healthcheck.StatusUpdater)
		if !ok {
			return nil, fmt.Errorf("child service %v of %v not a healthcheck.StatusUpdater (%T)", childName, serviceName, serviceHandler)
		}

		if err := updater.RegisterStatusUpdater(func(up bool) {
			balancer.SetStatus(ctx, childName, up)
		}); err != nil {
			return nil, fmt.Errorf("cannot register %v as updater for %v: %w", childName, serviceName, err)
		}

		log.Ctx(ctx).Debug().Str("parent", serviceName).Str("child", childName).
			Msg("Child service will update parent on status change")
	}

	return balancer, nil
}

func (m *Manager) getServiceHandler(ctx context.Context, service dynamic.WRRService) (http.Handler, error) {
	switch {
	case service.Status != nil:
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			rw.WriteHeader(*service.Status)
		}), nil

	case service.GRPCStatus != nil:
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			st := status.New(service.GRPCStatus.Code, service.GRPCStatus.Msg)

			body, err := json.Marshal(st.Proto())
			if err != nil {
				http.Error(rw, "Failed to marshal status to JSON", http.StatusInternalServerError)
				return
			}

			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusOK)

			_, _ = rw.Write(body)
		}), nil

	default:
		return m.BuildHTTP(ctx, service.Name)
	}
}
func (m *Manager) getHRWServiceHandler(ctx context.Context, serviceName string, config *dynamic.HighestRandomWeight) (http.Handler, error) {
	// TODO Handle accesslog and metrics with multiple service name
	balancer := hrw.New(config.HealthCheck != nil)
	for _, service := range shuffle(config.Services, m.rand) {
		serviceHandler, err := m.BuildHTTP(ctx, service.Name)
		if err != nil {
			return nil, err
		}

		balancer.Add(service.Name, serviceHandler, service.Weight, false)

		if config.HealthCheck == nil {
			continue
		}

		childName := service.Name
		updater, ok := serviceHandler.(healthcheck.StatusUpdater)
		if !ok {
			return nil, fmt.Errorf("child service %v of %v not a healthcheck.StatusUpdater (%T)", childName, serviceName, serviceHandler)
		}

		if err := updater.RegisterStatusUpdater(func(up bool) {
			balancer.SetStatus(ctx, childName, up)
		}); err != nil {
			return nil, fmt.Errorf("cannot register %v as updater for %v: %w", childName, serviceName, err)
		}

		log.Ctx(ctx).Debug().Str("parent", serviceName).Str("child", childName).
			Msg("Child service will update parent on status change")
	}

	return balancer, nil
}

func (m *Manager) getLoadBalancerServiceHandler(ctx context.Context, serviceName string, info *runtime.ServiceInfo) (http.Handler, error) {
	service := info.LoadBalancer

	logger := log.Ctx(ctx)
	logger.Debug().Msg("Creating load-balancer")

	// TODO: should we keep this config value as Go is now handling stream response correctly?
	flushInterval := time.Duration(dynamic.DefaultFlushInterval)
	if service.ResponseForwarding != nil {
		flushInterval = time.Duration(service.ResponseForwarding.FlushInterval)
	}

	if len(service.ServersTransport) > 0 {
		service.ServersTransport = provider.GetQualifiedName(ctx, service.ServersTransport)
	}

	if service.Sticky != nil && service.Sticky.Cookie != nil {
		service.Sticky.Cookie.Name = cookie.GetName(service.Sticky.Cookie.Name, serviceName)
	}

	// We make sure that the PassHostHeader value is defined to avoid panics.
	passHostHeader := dynamic.DefaultPassHostHeader
	if service.PassHostHeader != nil {
		passHostHeader = *service.PassHostHeader
	}

	// lb := wrr.New(service.Sticky, service.HealthCheck != nil)
	// roundTripper, err := m.roundTripperManager.Get(service.ServersTransport)
	// if err != nil {
	// 	return nil, err
	// }

	// here choose between WRR and HRW based on load balancer type
	var lbHRW *hrw.Balancer
	var lbWRR *wrr.Balancer
	if info.LoadBalancer.Type == "hrw" {
		lbHRW = hrw.New(service.HealthCheck != nil)
	} else {
		lbWRR = wrr.New(service.Sticky, service.HealthCheck != nil)
	}
	healthCheckTargets := make(map[string]*url.URL)

	for _, server := range shuffle(service.Servers, m.rand) {
		hasher := fnv.New64a()
		_, _ = hasher.Write([]byte(server.URL)) // this will never return an error.

		proxyName := hex.EncodeToString(hasher.Sum(nil))

		target, err := url.Parse(server.URL)
		if err != nil {
			return nil, fmt.Errorf("error parsing server URL %s: %w", server.URL, err)
		}

		logger.Debug().Str(logs.ServerName, proxyName).Stringer("target", target).
			Msg("Creating server")

		qualifiedSvcName := provider.GetQualifiedName(ctx, serviceName)

		shouldObserve := m.observabilityMgr.ShouldAddTracing(qualifiedSvcName, nil) || m.observabilityMgr.ShouldAddMetrics(qualifiedSvcName, nil)
		proxy, err := m.proxyBuilder.Build(service.ServersTransport, target, shouldObserve, passHostHeader, server.PreservePath, flushInterval)
		if err != nil {
			return nil, fmt.Errorf("error building proxy for server URL %s: %w", server.URL, err)
		}

		// Prevents from enabling observability for internal resources.

		if m.observabilityMgr.ShouldAddAccessLogs(qualifiedSvcName, nil) {
			proxy = accesslog.NewFieldHandler(proxy, accesslog.ServiceURL, target.String(), nil)
			proxy = accesslog.NewFieldHandler(proxy, accesslog.ServiceAddr, target.Host, nil)
			proxy = accesslog.NewFieldHandler(proxy, accesslog.ServiceName, serviceName, accesslog.AddServiceFields)
		}

		if m.observabilityMgr.MetricsRegistry() != nil && m.observabilityMgr.MetricsRegistry().IsSvcEnabled() &&
			m.observabilityMgr.ShouldAddMetrics(qualifiedSvcName, nil) {
			metricsHandler := metricsMiddle.WrapServiceHandler(ctx, m.observabilityMgr.MetricsRegistry(), serviceName)

			proxy, err = alice.New().
				Append(observability.WrapMiddleware(ctx, metricsHandler)).
				Then(proxy)
			if err != nil {
				return nil, fmt.Errorf("error wrapping metrics handler: %w", err)
			}
		}

		if m.observabilityMgr.ShouldAddTracing(qualifiedSvcName, nil) {
			proxy = observability.NewService(ctx, serviceName, proxy)
		}

		if m.observabilityMgr.ShouldAddAccessLogs(qualifiedSvcName, nil) || m.observabilityMgr.ShouldAddMetrics(qualifiedSvcName, nil) {
			// Some piece of middleware, like the ErrorPage, are relying on this serviceBuilder to get the handler for a given service,
			// to re-target the request to it.
			// Those pieces of middleware can be configured on routes that expose a Traefik internal service.
			// In such a case, observability for internals being optional, the capture probe could be absent from context (no wrap via the entrypoint).
			// But if the service targeted by this piece of middleware is not an internal one,
			// and requires observability, we still want the capture probe to be present in the request context.
			// Makes sure a capture probe is in the request context.
			proxy, _ = capture.Wrap(proxy)
		}

		// lb.Add(proxyName, proxy, server.Weight, server.Fenced)
		if info.LoadBalancer.Type == "hrw" {
			lbHRW.Add(proxyName, proxy, server.Weight, server.Fenced)
		} else {
			lbWRR.Add(proxyName, proxy, server.Weight, server.Fenced)
		}

		// servers are considered UP by default.
		info.UpdateServerStatus(target.String(), runtime.StatusUp)

		healthCheckTargets[proxyName] = target
	}

	roundTripper, err := m.transportManager.GetRoundTripper(service.ServersTransport)
	if err != nil {
		return nil, fmt.Errorf("getting RoundTripper: %w", err)
	}

	// fills healthcheck for the service of the loadbalancer
	if info.LoadBalancer.Type == "hrw" {
		if service.HealthCheck != nil {
			m.healthCheckers[serviceName] = healthcheck.NewServiceHealthChecker(
				ctx,
				m.observabilityMgr.MetricsRegistry(),
				service.HealthCheck,
				lbHRW,
				info,
				roundTripper,
				healthCheckTargets,
				serviceName,
			)
		}
		return lbHRW, nil
	}
	if service.HealthCheck != nil {
		m.healthCheckers[serviceName] = healthcheck.NewServiceHealthChecker(
			ctx,
			m.observabilityMgr.MetricsRegistry(),
			service.HealthCheck,
			lbWRR,
			info,
			roundTripper,
			healthCheckTargets,
			serviceName,
		)
	}
	return lbWRR, nil
}

// LaunchHealthCheck launches the health checks.
func (m *Manager) LaunchHealthCheck(ctx context.Context) {
	for serviceName, hc := range m.healthCheckers {
		logger := log.Ctx(ctx).With().Str(logs.ServiceName, serviceName).Logger()
		go hc.Launch(logger.WithContext(ctx))
	}
}

func shuffle[T any](values []T, r *rand.Rand) []T {
	shuffled := make([]T, len(values))
	copy(shuffled, values)
	r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	return shuffled
}
