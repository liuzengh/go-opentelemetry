//
//
// Tencent is pleased to support the open source community by making tRPC available.
//
// Copyright (C) 2023 THL A29 Limited, a Tencent company.
// All rights reserved.
//
// If you have downloaded a copy of the tRPC source code from Tencent,
// please note that tRPC source code is licensed under the  Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//

package asyncexporter

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"go.opentelemetry.io/otel"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logsproto "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"trpc-system/go-opentelemetry/exporter/retry"
	"trpc-system/go-opentelemetry/pkg/metrics"
	"trpc-system/go-opentelemetry/sdk/log"
)

var _ log.Exporter = (*Exporter)(nil)

// Exporter exports telemetry data from the collector to a destination.
type Exporter struct {
	// mu protects the non-atomic and non-channel variables
	mu      sync.RWMutex
	started bool

	logExporter collectorlogspb.LogsServiceClient

	grpcClientConn    *grpc.ClientConn
	lastConnectErrPtr unsafe.Pointer

	startOnce      sync.Once
	stopCh         chan bool
	disconnectedCh chan bool

	backgroundConnectionDoneCh chan bool

	c        config
	metadata metadata.MD

	logsBatchCh   chan []*logsproto.ResourceLogs
	logsBatchPool sync.Pool
	wait          sync.WaitGroup
}

// newConfig initializes a config struct with default values and applies
// any ExporterOptions provided.
func newConfig(opts ...ExporterOption) config {
	cfg := config{
		numWorkers:        DefaultNumWorkers,
		grpcServiceConfig: DefaultGRPCServiceConfig,
		concurrency:       Concurrency,
		requestFunc:       retry.Config{Enabled: false}.RequestFunc(retryable), // disable retry as default
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// NewExporter constructs a new Exporter and starts it.
func NewExporter(opts ...ExporterOption) (*Exporter, error) {
	exp := NewUnstartedExporter(opts...)
	if err := exp.Start(); err != nil {
		return nil, err
	}
	return exp, nil
}

// NewUnstartedExporter constructs a new Exporter and does not start it.
func NewUnstartedExporter(opts ...ExporterOption) *Exporter {
	e := new(Exporter)
	e.c = newConfig(opts...)
	if len(e.c.headers) > 0 {
		e.metadata = metadata.New(e.c.headers)
	}

	return e
}

func (e *Exporter) connected() bool {
	return e.lastConnectError() == nil
}

func (e *Exporter) lastConnectError() error {
	errPtr := (*error)(atomic.LoadPointer(&e.lastConnectErrPtr))
	if errPtr == nil {
		return nil
	}
	return *errPtr
}

// ExportLogs export log
func (e *Exporter) ExportLogs(parent context.Context, logs []*logsproto.ResourceLogs) error {
	defer func() {
		x := recover()
		switch err := x.(type) {
		case nil:
			return
		case runtime.Error:
			if err.Error() == "send on closed channel logsBatchCh" {
				return
			}
		}
		panic(x)
	}()

	batch := e.getBatchSlice()
	batch = append(batch, logs...)
	e.logsBatchCh <- batch
	return nil
}

func (e *Exporter) exportLogsInternal(parent context.Context, logs []*logsproto.ResourceLogs) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func(ctx context.Context, cancel context.CancelFunc) {
		select {
		case <-ctx.Done():
		case <-e.stopCh:
			cancel()
		}
	}(ctx, cancel)

	if len(logs) == 0 {
		return nil
	}

	if !e.connected() {
		return errDisconnected
	}

	select {
	case <-e.stopCh:
		return errStopped
	case <-ctx.Done():
		return errContextCanceled
	default:
		e.mu.RLock()
		err := e.c.requestFunc(e.contextWithMetadata(ctx), func(ctx context.Context) error {
			_, err := e.logExporter.Export(e.contextWithMetadata(ctx), &collectorlogspb.ExportLogsServiceRequest{
				ResourceLogs: logs,
			})
			if status.Code(err) == codes.OK {
				return nil
			}
			return err
		})
		e.mu.RUnlock()
		if err != nil {
			e.setStateDisconnected(err)
			return err
		}
	}
	return nil
}

var (
	errAlreadyStarted  = errors.New("already started")
	errNotStarted      = errors.New("not started")
	errDisconnected    = errors.New("exporter disconnected")
	errStopped         = errors.New("exporter stopped")
	errContextCanceled = errors.New("context canceled")
)

// Start dials to the collector, establishing a connection to it. It also
// initiates the Config and Trace services by sending over the initial
// messages that consist of the node identifier. Start invokes a background
// connector that will reattempt connections to the collector periodically
// if the connection dies.
func (e *Exporter) Start() error {
	var err = errAlreadyStarted
	e.startOnce.Do(func() {
		e.mu.Lock()
		e.started = true
		e.disconnectedCh = make(chan bool, 1)
		e.stopCh = make(chan bool)
		e.backgroundConnectionDoneCh = make(chan bool)
		e.logsBatchCh = make(chan []*logsproto.ResourceLogs, 2*e.c.concurrency)
		e.logsBatchPool = sync.Pool{
			New: func() interface{} {
				out := make([]*logsproto.ResourceLogs, 0, MaxExportBatchSize)
				return &out
			},
		}
		e.mu.Unlock()

		// An optimistic first connection attempt to ensure that
		// applications under heavy load can immediately process
		// data. See https://github.com/census-ecosystem/opencensus-go-exporter-ocagent/pull/63
		if err = e.connect(); err == nil {
			e.setStateConnected()
		} else {
			e.setStateDisconnected(err)
		}
		go e.indefiniteBackgroundConnection()

		err = nil
		for i := 0; i < e.c.concurrency; i++ {
			e.wait.Add(1)
			go func() {
				defer e.wait.Done()
				e.exportBatches()
			}()
		}
	})

	return err
}

// closeStopCh is used to wrap the exporters stopCh channel closing for testing.
var closeStopCh = func(stopCh chan bool) {
	close(stopCh)
}

// Shutdown closes all connections and releases resources currently being used
// by the exporter. If the exporter is not started this does nothing.
func (e *Exporter) Shutdown(ctx context.Context) error {
	e.mu.RLock()
	cc := e.grpcClientConn
	started := e.started
	e.mu.RUnlock()

	if !started {
		return nil
	}

	close(e.logsBatchCh)

	// waiting for the logs entered into the queue to be sent.
	e.wait.Wait()
	var err error
	if cc != nil {
		// Clean things up before checking this error.
		err = cc.Close()
	}

	// At this point we can change the state variable started
	e.mu.Lock()
	e.started = false
	e.mu.Unlock()
	closeStopCh(e.stopCh)

	// Ensure that the backgroundConnector returns
	select {
	case <-e.backgroundConnectionDoneCh:
	case <-ctx.Done():
		return ctx.Err()
	}

	return err
}

func (e *Exporter) exportBatches() {
	for batch := range e.logsBatchCh {
		ctx := context.Background()
		size := len(batch)
		err := e.exportLogsInternal(ctx, batch)
		if err != nil {
			otel.Handle(err)
			metrics.BatchProcessCounter.WithLabelValues("async_failed", "logs").Add(float64(size))
		} else {
			metrics.BatchProcessCounter.WithLabelValues("async_success", "logs").Add(float64(size))
		}
		e.putBatchSlice(batch)
	}
}

const defaultConnReattemptPeriod = 10 * time.Second

func (e *Exporter) indefiniteBackgroundConnection() {
	defer func() {
		e.backgroundConnectionDoneCh <- true
	}()

	connReattemptPeriod := e.c.reconnectionPeriod
	if connReattemptPeriod <= 0 {
		connReattemptPeriod = defaultConnReattemptPeriod
	}

	// No strong seeding required, nano time can
	// already help with pseudo uniqueness.
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + rand.Int63n(1024)))

	// maxJitterNanos: 70% of the connectionReattemptPeriod
	maxJitterNanos := int64(0.7 * float64(connReattemptPeriod))

	for {
		// Otherwise these will be the normal scenarios to enable
		// reconnection if we trip out.
		// 1. If we've stopped, return entirely
		// 2. Otherwise block until we are disconnected, and
		//    then retry connecting
		select {
		case <-e.stopCh:
			return

		case <-e.disconnectedCh:
			// Normal scenario that we'll wait for
		}

		if err := e.connect(); err == nil {
			e.setStateConnected()
		} else {
			e.setStateDisconnected(err)
		}

		// Apply some jitter to avoid lockstep retrials of other
		// collector-exporters. Lockstep retrials could result in an
		// innocent DDOS, by clogging the machine's resources and network.
		jitter := time.Duration(rng.Int63n(maxJitterNanos))
		select {
		case <-e.stopCh:
			return
		case <-time.After(connReattemptPeriod + jitter):
		}
	}
}

func (e *Exporter) setStateConnected() {
	e.saveLastConnectError(nil)
}

func (e *Exporter) setStateDisconnected(err error) {
	e.saveLastConnectError(err)
	select {
	case e.disconnectedCh <- true:
	default:
	}
}

func (e *Exporter) saveLastConnectError(err error) {
	var errPtr *error
	if err != nil {
		errPtr = &err
	}
	atomic.StorePointer(&e.lastConnectErrPtr, unsafe.Pointer(errPtr))
}

func (e *Exporter) connect() error {
	cc, err := e.dialToCollector()
	if err != nil {
		return err
	}
	return e.enableConnections(cc)
}

func (e *Exporter) prepareCollectorAddress() string {
	if e.c.collectorAddr != "" {
		return e.c.collectorAddr
	}
	return fmt.Sprintf("%s:%d", DefaultCollectorHost, DefaultCollectorPort)
}

func (e *Exporter) dialToCollector() (*grpc.ClientConn, error) {
	addr := e.prepareCollectorAddress()

	dialOpts := []grpc.DialOption{}
	if e.c.grpcServiceConfig != "" {
		dialOpts = append(dialOpts, grpc.WithDefaultServiceConfig(e.c.grpcServiceConfig))
	}
	if e.c.clientCredentials != nil {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(e.c.clientCredentials))
	} else {
		// insecure as default
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	if e.c.compressor != "" {
		dialOpts = append(dialOpts, grpc.WithDefaultCallOptions(grpc.UseCompressor(e.c.compressor)))
	}
	if len(e.c.grpcDialOptions) != 0 {
		dialOpts = append(dialOpts, e.c.grpcDialOptions...)
	}

	ctx := e.contextWithMetadata(context.Background())
	return grpc.DialContext(ctx, addr, dialOpts...)
}

func (e *Exporter) contextWithMetadata(ctx context.Context) context.Context {
	if e.metadata.Len() > 0 {
		return metadata.NewOutgoingContext(ctx, e.metadata)
	}
	return ctx
}

func (e *Exporter) enableConnections(cc *grpc.ClientConn) error {
	e.mu.RLock()
	started := e.started
	e.mu.RUnlock()

	if !started {
		return errNotStarted
	}

	e.mu.Lock()
	// If previous clientConn is same as the current then just return.
	// This doesn't happen right now as this func is only called with new ClientConn.
	// It is more about future-proofing.
	if e.grpcClientConn == cc {
		e.mu.Unlock()
		return nil
	}
	// If the previous clientConn was non-nil, close it
	if e.grpcClientConn != nil {
		_ = e.grpcClientConn.Close()
	}
	e.grpcClientConn = cc
	e.logExporter = collectorlogspb.NewLogsServiceClient(cc)
	e.mu.Unlock()

	return nil
}

func (e *Exporter) getBatchSlice() []*logsproto.ResourceLogs {
	batch, _ := e.logsBatchPool.Get().(*[]*logsproto.ResourceLogs)
	return (*batch)[:0]
}

func (e *Exporter) putBatchSlice(batch []*logsproto.ResourceLogs) {
	e.logsBatchPool.Put(&batch)
}

// retryable returns if err identifies a request that can be retried and a
// duration to wait for if an explicit throttle time is included in err.
func retryable(err error) (bool, time.Duration) {
	s := status.Convert(err)
	switch s.Code() {
	case codes.Canceled,
		codes.DeadlineExceeded,
		codes.ResourceExhausted,
		codes.Aborted,
		codes.OutOfRange,
		codes.Unavailable,
		codes.DataLoss:
		return true, throttleDelay(s)
	}

	// Not a retry-able error.
	return false, 0
}

// throttleDelay returns a duration to wait for if an explicit throttle time
// is included in the response status.
func throttleDelay(s *status.Status) time.Duration {
	for _, detail := range s.Details() {
		if t, ok := detail.(*errdetails.RetryInfo); ok {
			return t.RetryDelay.AsDuration()
		}
	}
	return 0
}
