// Copyright 2018 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package internal

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	ext_core_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	ext_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ext_authz_v2 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v2"
	ext_authz_v3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	ext_type_v2 "github.com/envoyproxy/go-control-plane/envoy/type"
	ext_type_v3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/genproto/googleapis/rpc/code"
	rpc_status "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/config"
	"github.com/open-policy-agent/opa/logging"
	"github.com/open-policy-agent/opa/plugins"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/server"
	"github.com/open-policy-agent/opa/storage"
	iCache "github.com/open-policy-agent/opa/topdown/cache"
	"github.com/open-policy-agent/opa/tracing"
	"github.com/open-policy-agent/opa/util"

	"github.com/open-policy-agent/opa-envoy-plugin/envoyauth"
	internal_util "github.com/open-policy-agent/opa-envoy-plugin/internal/util"
	"github.com/open-policy-agent/opa-envoy-plugin/opa/decisionlog"
)

const (
	defaultAddr                     = ":9191"
	defaultPath                     = "envoy/authz/allow"
	defaultDryRun                   = false
	defaultEnableReflection         = false
	defaultSkipRequestBodyParse     = false
	defaultEnablePerformanceMetrics = false

	// Those are the defaults from grpc-go.
	// See https://github.com/grpc/grpc-go/blob/master/server.go#L58 for more details.
	defaultGRPCServerMaxReceiveMessageSize = 1024 * 1024 * 4
	defaultGRPCServerMaxSendMessageSize    = math.MaxInt32

	// PluginName is the name to register with the OPA plugin manager
	PluginName = "envoy_ext_authz_grpc"
)

// Validate receives a slice of bytes representing the plugin's
// configuration and returns a configuration value that can be used to
// instantiate the plugin.
func Validate(m *plugins.Manager, bs []byte) (*Config, error) {
	cfg := Config{
		Addr:                     defaultAddr,
		DryRun:                   defaultDryRun,
		EnableReflection:         defaultEnableReflection,
		GRPCMaxRecvMsgSize:       defaultGRPCServerMaxReceiveMessageSize,
		GRPCMaxSendMsgSize:       defaultGRPCServerMaxSendMessageSize,
		SkipRequestBodyParse:     defaultSkipRequestBodyParse,
		EnablePerformanceMetrics: defaultEnablePerformanceMetrics,
	}

	if err := util.Unmarshal(bs, &cfg); err != nil {
		return nil, err
	}

	if cfg.Path != "" && cfg.Query != "" {
		return nil, fmt.Errorf("invalid config: specify a value for only the \"path\" field")
	}

	var parsedQuery ast.Body
	var err error

	if cfg.Query != "" {
		// Deprecated: Use Path instead
		parsedQuery, err = ast.ParseBody(cfg.Query)
	} else {
		if cfg.Path == "" {
			cfg.Path = defaultPath
		}
		path := stringPathToDataRef(cfg.Path)
		parsedQuery, err = ast.ParseBody(path.String())
	}

	if err != nil {
		return nil, err
	}

	cfg.parsedQuery = parsedQuery

	if cfg.ProtoDescriptor != "" {
		ps, err := internal_util.ReadProtoSet(cfg.ProtoDescriptor)
		if err != nil {
			return nil, err
		}
		cfg.protoSet = ps
	}

	return &cfg, nil
}

// New returns a Plugin that implements the Envoy ext_authz API.
func New(m *plugins.Manager, cfg *Config) plugins.Plugin {
	grpcOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(cfg.GRPCMaxRecvMsgSize),
		grpc.MaxSendMsgSize(cfg.GRPCMaxSendMsgSize),
	}
	var distributedTracingOpts tracing.Options = nil
	if m.TracerProvider() != nil {
		grpcTracingOption := []otelgrpc.Option{
			otelgrpc.WithTracerProvider(m.TracerProvider()),
			otelgrpc.WithPropagators(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})),
		}
		distributedTracingOpts = tracing.NewOptions(
			otelhttp.WithTracerProvider(m.TracerProvider()),
			otelhttp.WithPropagators(propagation.TraceContext{}),
		)
		grpcOpts = append(grpcOpts,
			grpc.UnaryInterceptor(otelgrpc.UnaryServerInterceptor(grpcTracingOption...)),
			grpc.StreamInterceptor(otelgrpc.StreamServerInterceptor(grpcTracingOption...)),
		)
	}

	plugin := &envoyExtAuthzGrpcServer{
		manager:                m,
		cfg:                    *cfg,
		server:                 grpc.NewServer(grpcOpts...),
		preparedQueryDoOnce:    new(sync.Once),
		interQueryBuiltinCache: iCache.NewInterQueryCache(m.InterQueryBuiltinCacheConfig()),
		distributedTracingOpts: distributedTracingOpts,
	}

	// Register Authorization Server
	ext_authz_v3.RegisterAuthorizationServer(plugin.server, plugin)
	ext_authz_v2.RegisterAuthorizationServer(plugin.server, &envoyExtAuthzV2Wrapper{v3: plugin})

	m.RegisterCompilerTrigger(plugin.compilerUpdated)

	// Register reflection service on gRPC server
	if cfg.EnableReflection {
		reflection.Register(plugin.server)
	}
	if cfg.EnablePerformanceMetrics {
		histogramAuthzDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "grpc_request_duration_seconds",
			Help: "A histogram of duration for grpc authz requests.",
			Buckets: []float64{
				1e-6,
				5e-6,
				1e-5,
				5e-5,
				1e-4,
				5e-4,
				1e-3,
				3e-3,
				5e-3,
				0.1,
				1,
			},
		}, []string{"handler"})
		plugin.metricAuthzDuration = *histogramAuthzDuration
		plugin.manager.PrometheusRegister().MustRegister(histogramAuthzDuration)
	}

	m.UpdatePluginStatus(PluginName, &plugins.Status{State: plugins.StateNotReady})

	return plugin
}

// Config represents the plugin configuration.
type Config struct {
	Addr                     string `json:"addr"`
	Query                    string `json:"query"` // Deprecated: Use Path instead
	Path                     string `json:"path"`
	DryRun                   bool   `json:"dry-run"`
	EnableReflection         bool   `json:"enable-reflection"`
	parsedQuery              ast.Body
	ProtoDescriptor          string `json:"proto-descriptor"`
	protoSet                 *protoregistry.Files
	GRPCMaxRecvMsgSize       int  `json:"grpc-max-recv-msg-size"`
	GRPCMaxSendMsgSize       int  `json:"grpc-max-send-msg-size"`
	SkipRequestBodyParse     bool `json:"skip-request-body-parse"`
	EnablePerformanceMetrics bool `json:"enable-performance-metrics"`
}

type envoyExtAuthzGrpcServer struct {
	cfg                    Config
	server                 *grpc.Server
	manager                *plugins.Manager
	preparedQuery          *rego.PreparedEvalQuery
	preparedQueryDoOnce    *sync.Once
	interQueryBuiltinCache iCache.InterQueryCache
	distributedTracingOpts tracing.Options
	metricAuthzDuration    prometheus.HistogramVec
}

type envoyExtAuthzV2Wrapper struct {
	v3 *envoyExtAuthzGrpcServer
}

func (p *envoyExtAuthzGrpcServer) ParsedQuery() ast.Body {
	return p.cfg.parsedQuery
}

func (p *envoyExtAuthzGrpcServer) Store() storage.Store {
	return p.manager.Store
}

func (p *envoyExtAuthzGrpcServer) Compiler() *ast.Compiler {
	return p.manager.GetCompiler()
}

func (p *envoyExtAuthzGrpcServer) Config() *config.Config {
	return p.manager.Config
}

func (p *envoyExtAuthzGrpcServer) Runtime() *ast.Term {
	return p.manager.Info
}

func (p *envoyExtAuthzGrpcServer) PreparedQueryDoOnce() *sync.Once {
	return p.preparedQueryDoOnce
}

func (p *envoyExtAuthzGrpcServer) InterQueryBuiltinCache() iCache.InterQueryCache {
	return p.interQueryBuiltinCache
}

func (p *envoyExtAuthzGrpcServer) PreparedQuery() *rego.PreparedEvalQuery {
	return p.preparedQuery
}

func (p *envoyExtAuthzGrpcServer) SetPreparedQuery(pq *rego.PreparedEvalQuery) {
	p.preparedQuery = pq
}

func (p *envoyExtAuthzGrpcServer) Logger() logging.Logger {
	return p.manager.Logger()
}

func (p *envoyExtAuthzGrpcServer) DistributedTracing() tracing.Options {
	return p.distributedTracingOpts
}

func (p *envoyExtAuthzGrpcServer) Start(ctx context.Context) error {
	p.manager.UpdatePluginStatus(PluginName, &plugins.Status{State: plugins.StateNotReady})
	go p.listen()
	return nil
}

func (p *envoyExtAuthzGrpcServer) Stop(ctx context.Context) {
	p.server.Stop()
	p.manager.UpdatePluginStatus(PluginName, &plugins.Status{State: plugins.StateNotReady})
}

func (p *envoyExtAuthzGrpcServer) Reconfigure(ctx context.Context, config interface{}) {
	return
}

func (p *envoyExtAuthzGrpcServer) compilerUpdated(txn storage.Transaction) {
	p.preparedQueryDoOnce = new(sync.Once)
}

func (p *envoyExtAuthzGrpcServer) listen() {
	logger := p.manager.Logger()
	addr := p.cfg.Addr
	if !strings.Contains(addr, "://") {
		addr = "grpc://" + addr
	}

	parsedURL, err := url.Parse(addr)
	if err != nil {
		logger.WithFields(map[string]interface{}{"err": err}).Error("Unable to parse url.")
		return
	}

	// The listener is closed automatically by Serve when it returns.
	var l net.Listener

	switch parsedURL.Scheme {
	case "unix":
		socketPath := parsedURL.Host + parsedURL.Path
		// Recover @ prefix for abstract Unix sockets.
		if strings.HasPrefix(parsedURL.String(), parsedURL.Scheme+"://@") {
			socketPath = "@" + socketPath
		} else {
			// Remove domain socket file in case it already exists.
			os.Remove(socketPath)
		}
		l, err = net.Listen("unix", socketPath)
	case "grpc":
		l, err = net.Listen("tcp", parsedURL.Host)
	default:
		err = fmt.Errorf("invalid url scheme %q", parsedURL.Scheme)
	}

	if err != nil {
		logger.WithFields(map[string]interface{}{"err": err}).Error("Unable to create listener.")
	}

	logger.WithFields(map[string]interface{}{
		"addr":              p.cfg.Addr,
		"query":             p.cfg.Query,
		"path":              p.cfg.Path,
		"dry-run":           p.cfg.DryRun,
		"enable-reflection": p.cfg.EnableReflection,
	}).Info("Starting gRPC server.")

	p.manager.UpdatePluginStatus(PluginName, &plugins.Status{State: plugins.StateOK})

	if err := p.server.Serve(l); err != nil {
		logger.WithFields(map[string]interface{}{"err": err}).Error("Listener failed.")
		return
	}

	logger.Info("Listener exited.")
	p.manager.UpdatePluginStatus(PluginName, &plugins.Status{State: plugins.StateNotReady})
}

// Check is envoy.service.auth.v3.Authorization/Check
func (p *envoyExtAuthzGrpcServer) Check(ctx context.Context, req *ext_authz_v3.CheckRequest) (*ext_authz_v3.CheckResponse, error) {
	resp, stop, err := p.check(ctx, req)
	if code := stop(); resp != nil && code != nil {
		resp.Status = code
	}
	return resp, err
}

func (p *envoyExtAuthzGrpcServer) check(ctx context.Context, req interface{}) (*ext_authz_v3.CheckResponse, func() *rpc_status.Status, error) {
	var err error
	var evalErr error
	start := time.Now()
	logger := p.manager.Logger()

	result, stopeval, err := envoyauth.NewEvalResult()
	if err != nil {
		logger.WithFields(map[string]interface{}{"err": err}).Error("Unable to start new evaluation.")
		return nil, func() *rpc_status.Status { return nil }, err
	}

	txn, txnClose, err := result.GetTxn(ctx, p.Store())
	if err != nil {
		logger.WithFields(map[string]interface{}{"err": err}).Error("Unable to start new storage transaction.")
		return nil, func() *rpc_status.Status { return nil }, err
	}

	result.Txn = txn

	logger = logger.WithFields(map[string]interface{}{"decision-id": result.DecisionID})

	var input map[string]interface{}

	stop := func() *rpc_status.Status {
		stopeval()
		logErr := p.log(ctx, input, result, err)
		if logErr != nil {
			_ = txnClose(ctx, logErr) // Ignore error
			return &rpc_status.Status{
				Code:    int32(code.Code_UNKNOWN),
				Message: logErr.Error(),
			}
		}
		_ = txnClose(ctx, evalErr) // Ignore error
		return nil
	}

	if ctx.Err() != nil {
		err = errors.Wrap(ctx.Err(), "check request timed out before query execution")
		return nil, stop, err
	}

	input, err = envoyauth.RequestToInput(req, logger, p.cfg.protoSet, p.cfg.SkipRequestBodyParse)
	if err != nil {
		return nil, stop, err
	}

	var inputValue ast.Value
	inputValue, err = ast.InterfaceToValue(input)
	if err != nil {
		return nil, stop, err
	}

	if err = envoyauth.Eval(ctx, p, inputValue, result); err != nil {
		evalErr = err
		return nil, stop, err
	}

	resp := &ext_authz_v3.CheckResponse{}

	var allowed bool
	allowed, err = result.IsAllowed()
	if err != nil {
		return nil, stop, errors.Wrap(err, "failed to get response status")
	}

	status := int32(code.Code_PERMISSION_DENIED)
	if allowed {
		status = int32(code.Code_OK)
	}
	resp.Status = &rpc_status.Status{Code: status}

	switch result.Decision.(type) {
	case map[string]interface{}:
		var responseHeaders []*ext_core_v3.HeaderValueOption
		responseHeaders, err = result.GetResponseEnvoyHeaderValueOptions()
		if err != nil {
			return nil, stop, errors.Wrap(err, "failed to get response headers")
		}

		dynamicMetadata, err := result.GetDynamicMetadata()
		if err != nil {
			return nil, stop, errors.Wrap(err, "failed to get dynamic metadata")
		}
		resp.DynamicMetadata = dynamicMetadata

		if status == int32(code.Code_OK) {
			var headersToRemove []string
			headersToRemove, err = result.GetRequestHTTPHeadersToRemove()
			if err != nil {
				return nil, stop, errors.Wrap(err, "failed to get request headers to remove")
			}

			var responseHeadersToAdd []*ext_core_v3.HeaderValueOption
			responseHeadersToAdd, err = result.GetResponseHTTPHeadersToAdd()
			if err != nil {
				return nil, stop, errors.Wrap(err, "failed to get response headers to send to client")
			}

			resp.HttpResponse = &ext_authz_v3.CheckResponse_OkResponse{
				OkResponse: &ext_authz_v3.OkHttpResponse{
					Headers:              responseHeaders,
					HeadersToRemove:      headersToRemove,
					ResponseHeadersToAdd: responseHeadersToAdd,
				},
			}
		} else {
			var body string
			body, err = result.GetResponseBody()
			if err != nil {
				return nil, stop, errors.Wrap(err, "failed to get response body")
			}

			var httpStatus *ext_type_v3.HttpStatus
			httpStatus, err = result.GetResponseEnvoyHTTPStatus()
			if err != nil {
				return nil, stop, errors.Wrap(err, "failed to get response http status")
			}

			deniedResponse := &ext_authz_v3.DeniedHttpResponse{
				Headers: responseHeaders,
				Body:    body,
				Status:  httpStatus,
			}

			resp.HttpResponse = &ext_authz_v3.CheckResponse_DeniedResponse{
				DeniedResponse: deniedResponse,
			}
		}
	}

	totalDecisionTime := time.Since(start)

	if p.cfg.EnablePerformanceMetrics {
		p.metricAuthzDuration.
			With(prometheus.Labels{"handler": "check"}).
			Observe(float64(totalDecisionTime.Seconds()))
	}

	p.manager.Logger().WithFields(map[string]interface{}{
		"query":               p.cfg.parsedQuery.String(),
		"dry-run":             p.cfg.DryRun,
		"decision":            result.Decision,
		"err":                 err,
		"txn":                 result.TxnID,
		"metrics":             result.Metrics.All(),
		"total_decision_time": totalDecisionTime,
	}).Debug("Returning policy decision.")

	// If dry-run mode, override the Status code to unconditionally Allow the request
	// DecisionLogging should reflect what "would" have happened
	if p.cfg.DryRun {
		if resp.Status.Code != int32(code.Code_OK) {
			resp.Status = &rpc_status.Status{Code: int32(code.Code_OK)}
			resp.HttpResponse = &ext_authz_v3.CheckResponse_OkResponse{
				OkResponse: &ext_authz_v3.OkHttpResponse{},
			}
		}
	}

	return resp, stop, nil
}

func (p *envoyExtAuthzGrpcServer) log(ctx context.Context, input interface{}, result *envoyauth.EvalResult, err error) error {
	info := &server.Info{
		Timestamp: time.Now(),
		Input:     &input,
	}

	if p.cfg.Query != "" {
		info.Query = p.cfg.Query
	}

	if p.cfg.Path != "" {
		info.Path = p.cfg.Path
	}

	if result.NDBuiltinCache != nil {
		x, err := ast.JSON(result.NDBuiltinCache.AsValue())
		if err != nil {
			return err
		}
		info.NDBuiltinCache = &x
	}

	return decisionlog.LogDecision(ctx, p.manager, info, result, err)
}

func stringPathToDataRef(s string) (r ast.Ref) {
	result := ast.Ref{ast.DefaultRootDocument}
	result = append(result, stringPathToRef(s)...)
	return result
}

func stringPathToRef(s string) (r ast.Ref) {
	if len(s) == 0 {
		return r
	}

	p := strings.Split(s, "/")
	for _, x := range p {
		if x == "" {
			continue
		}

		i, err := strconv.Atoi(x)
		if err != nil {
			r = append(r, ast.StringTerm(x))
		} else {
			r = append(r, ast.IntNumberTerm(i))
		}
	}
	return r
}

// Check is envoy.service.auth.v2.Authorization/Check
func (p *envoyExtAuthzV2Wrapper) Check(ctx context.Context, req *ext_authz_v2.CheckRequest) (*ext_authz_v2.CheckResponse, error) {
	var stop func() *rpc_status.Status
	respV2 := &ext_authz_v2.CheckResponse{}
	respV3, stop, err := p.v3.check(ctx, req)
	defer func() {
		if code := stop(); code != nil {
			respV2.Status = code
		}
	}()

	if err != nil {
		return nil, err
	}
	respV2 = v2Response(respV3)
	return respV2, nil
}

func v2Response(respV3 *ext_authz_v3.CheckResponse) *ext_authz_v2.CheckResponse {
	respV2 := ext_authz_v2.CheckResponse{
		Status: respV3.Status,
	}
	switch http3 := respV3.HttpResponse.(type) {
	case *ext_authz_v3.CheckResponse_OkResponse:
		hdrs := http3.OkResponse.GetHeaders()
		respV2.HttpResponse = &ext_authz_v2.CheckResponse_OkResponse{
			OkResponse: &ext_authz_v2.OkHttpResponse{
				Headers: v2Headers(hdrs),
			},
		}
	case *ext_authz_v3.CheckResponse_DeniedResponse:
		hdrs := http3.DeniedResponse.GetHeaders()
		respV2.HttpResponse = &ext_authz_v2.CheckResponse_DeniedResponse{
			DeniedResponse: &ext_authz_v2.DeniedHttpResponse{
				Headers: v2Headers(hdrs),
				Status:  v2Status(http3.DeniedResponse.Status),
				Body:    http3.DeniedResponse.Body,
			},
		}
	}
	return &respV2
}

func v2Headers(hdrs []*ext_core_v3.HeaderValueOption) []*ext_core_v2.HeaderValueOption {
	hdrsV2 := make([]*ext_core_v2.HeaderValueOption, len(hdrs))
	for i, hv := range hdrs {
		hdrsV2[i] = &ext_core_v2.HeaderValueOption{
			Header: &ext_core_v2.HeaderValue{
				Key:   hv.GetHeader().Key,
				Value: hv.GetHeader().Value,
			},
		}
	}
	return hdrsV2
}

func v2Status(s *ext_type_v3.HttpStatus) *ext_type_v2.HttpStatus {
	return &ext_type_v2.HttpStatus{
		Code: ext_type_v2.StatusCode(s.Code),
	}
}
