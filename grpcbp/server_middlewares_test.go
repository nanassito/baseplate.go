package grpcbp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	pb "github.com/grpc-ecosystem/go-grpc-middleware/testing/testproto"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/reddit/baseplate.go/ecinterface"
	"github.com/reddit/baseplate.go/internal/prometheusbpint/spectest"
	"github.com/reddit/baseplate.go/mqsend"
	"github.com/reddit/baseplate.go/prometheusbp/promtest"
	"github.com/reddit/baseplate.go/tracing"
	"github.com/reddit/baseplate.go/transport"
)

const (
	testTimeout = time.Millisecond * 100
)

func TestInjectServerSpanInterceptorUnary(t *testing.T) {
	// create test server with InjectServerSpanInterceptor
	l, _ := setupServer(t, grpc.UnaryInterceptor(InjectServerSpanInterceptorUnary()))

	// create test client
	conn := setupClient(t, l)

	// instantiate gRPC client
	client := pb.NewTestServiceClient(conn)

	// set up recorder to validate span recording
	mmq := initTracing(t)

	t.Run("span-success", func(t *testing.T) {
		// send request to server
		if _, err := client.Ping(context.Background(), &pb.PingRequest{}); err != nil {
			t.Fatalf("Ping: %v", err)
		}

		// drain recorder to validate spans
		msg := drainRecorder(t, mmq)

		var trace tracing.ZipkinSpan
		if err := json.Unmarshal(msg, &trace); err != nil {
			t.Fatalf("recorded invalid JSON: %v", err)
		}

		got := trace.Name
		want := "Ping"
		if got != want {
			t.Errorf("got %s, want: %s", got, want)
		}

		for _, annotation := range trace.BinaryAnnotations {
			if annotation.Key == "error" {
				t.Error("got error span, want: success span")
				break
			}
		}
	})

	t.Run("span-error", func(t *testing.T) {
		// send request to server
		if _, err := client.PingError(context.Background(), &pb.PingRequest{}); err == nil {
			t.Error("PingError: got no error, want error")
		}

		// drain recorder to validate spans
		msg := drainRecorder(t, mmq)

		var trace tracing.ZipkinSpan
		if err := json.Unmarshal(msg, &trace); err != nil {
			t.Fatalf("recorded invalid JSON: %v", err)
		}

		got := trace.Name
		want := "PingError"
		if got != want {
			t.Errorf("got %s, want: %s", got, want)
		}

		for _, annotation := range trace.BinaryAnnotations {
			if annotation.Key == "error" {
				return
			}
		}
		t.Error("got success span, want: error span")
	})
}

func TestInjectEdgeContextInterceptorUnary(t *testing.T) {
	impl := ecinterface.Mock()

	// create test server with InjectServerSpanInterceptor
	l, service := setupServer(t, grpc.UnaryInterceptor(
		InjectEdgeContextInterceptorUnary(impl),
	))

	// create test client
	conn := setupClient(t, l, grpc.WithUnaryInterceptor(
		ForwardEdgeContextUnary(impl),
	))

	// instantiate gRPC client
	client := pb.NewTestServiceClient(conn)

	// create edge context
	ctx, err := impl.HeaderToContext(context.Background(), "dummy-edge-context")
	if err != nil {
		t.Fatalf("HeaderToContext: %v", err)
	}

	if _, err := client.Ping(ctx, &pb.PingRequest{}); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	ctx = service.ctx
	if ctx == nil {
		t.Error("got nil context")
	}

	if md, ok := metadata.FromIncomingContext(ctx); ok {
		value, ok := GetHeader(md, transport.HeaderEdgeRequest)
		if !ok {
			t.Fatal("header not set")
		}
		want := "dummy-edge-context"
		got := value
		if got != want {
			t.Errorf("got %s, want: %s", got, want)
		}
	}
}

func initTracing(t *testing.T) *mqsend.MockMessageQueue {
	t.Helper()

	t.Cleanup(func() {
		tracing.CloseTracer()
		tracing.InitGlobalTracer(tracing.Config{})
	})
	mmq := mqsend.OpenMockMessageQueue(mqsend.MessageQueueConfig{
		MaxQueueSize:   100,
		MaxMessageSize: 1024,
	})
	logger, startFailing := tracing.TestWrapper(t)
	tracing.InitGlobalTracer(tracing.Config{
		SampleRate:               1,
		MaxRecordTimeout:         testTimeout,
		Logger:                   logger,
		TestOnlyMockMessageQueue: mmq,
	})
	startFailing()
	return mmq
}

type mockService struct {
	ctx context.Context
}

func (t *mockService) Ping(ctx context.Context, req *pb.PingRequest) (*pb.PingResponse, error) {
	t.ctx = ctx
	return &pb.PingResponse{}, nil
}

func (t *mockService) PingError(ctx context.Context, req *pb.PingRequest) (*pb.Empty, error) {
	t.ctx = ctx
	return nil, errors.New("error")
}

func (t *mockService) PingEmpty(ctx context.Context, req *pb.Empty) (*pb.PingResponse, error) {
	panic("not implemented")
}

func (t *mockService) PingList(req *pb.PingRequest, c pb.TestService_PingListServer) error {
	panic("not implemented")
}
func (t *mockService) PingStream(c pb.TestService_PingStreamServer) error {
	panic("not implemented")
}

func TestInjectPrometheusUnaryServerClientInterceptor(t *testing.T) {
	const (
		serviceName = "mwitkow.testproto.TestService"
		serverSlug  = "example-preference-server"
		method      = "Ping"
	)
	// create test server with InjectPrometheusUnaryServerInterceptor
	l, service := setupServer(t, grpc.UnaryInterceptor(
		InjectPrometheusUnaryServerInterceptor(),
	))

	// create test client
	conn := setupClient(t, l, grpc.WithUnaryInterceptor(
		PrometheusUnaryClientInterceptor(serverSlug),
	))

	// instantiate gRPC client
	client := pb.NewTestServiceClient(conn)

	testCases := []struct {
		name    string
		wantErr codes.Code
		code    string
		success string
		method  string
	}{
		{
			name:    "success",
			wantErr: codes.OK,
			code:    "OK",
			success: "true",
			method:  "Ping",
		},
		{
			name:    "err",
			wantErr: codes.Internal,
			code:    "Unknown",
			success: "false",
			method:  "PingError",
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			serverLatencyDistribution.Reset()
			serverTotalRequests.Reset()
			serverActiveRequests.Reset()
			clientLatencyDistribution.Reset()
			clientTotalRequests.Reset()
			clientActiveRequests.Reset()

			serverLatencyLabels := prometheus.Labels{
				serviceLabel: serviceName,
				methodLabel:  tt.method,
				typeLabel:    unary,
				successLabel: tt.success,
			}

			serverTotalRequestLabels := prometheus.Labels{
				serviceLabel: serviceName,
				methodLabel:  tt.method,
				typeLabel:    unary,
				successLabel: tt.success,
				codeLabel:    tt.code,
			}

			serverActiveRequestLabels := prometheus.Labels{
				serviceLabel: serviceName,
				methodLabel:  tt.method,
			}

			clientLatencyLabels := prometheus.Labels{
				serviceLabel:    serviceName,
				methodLabel:     tt.method,
				typeLabel:       unary,
				successLabel:    tt.success,
				clientNameLabel: serverSlug,
			}

			clientTotalRequestLabels := prometheus.Labels{
				serviceLabel:    serviceName,
				methodLabel:     tt.method,
				typeLabel:       unary,
				successLabel:    tt.success,
				clientNameLabel: serverSlug,
				codeLabel:       tt.code,
			}

			clientActiveRequestLabels := prometheus.Labels{
				serviceLabel:    serviceName,
				methodLabel:     tt.method,
				clientNameLabel: serverSlug,
			}

			defer promtest.NewPrometheusMetricTest(t, "server latency", serverLatencyDistribution, serverLatencyLabels).CheckSampleCountDelta(1)
			defer promtest.NewPrometheusMetricTest(t, "server rpc count", serverTotalRequests, serverTotalRequestLabels).CheckDelta(1)
			defer promtest.NewPrometheusMetricTest(t, "server active requests", serverActiveRequests, serverActiveRequestLabels).CheckDelta(0)
			defer promtest.NewPrometheusMetricTest(t, "client latency", clientLatencyDistribution, clientLatencyLabels).CheckSampleCountDelta(1)
			defer promtest.NewPrometheusMetricTest(t, "client rpc count", clientTotalRequests, clientTotalRequestLabels).CheckDelta(1)
			defer promtest.NewPrometheusMetricTest(t, "client active requests", clientActiveRequests, clientActiveRequestLabels).CheckDelta(0)
			defer spectest.ValidateSpec(t, "grpc", "server")
			defer spectest.ValidateSpec(t, "grpc", "client")

			ctx := context.Background()
			if tt.success == "true" {
				if _, err := client.Ping(ctx, &pb.PingRequest{}); err != nil {
					t.Fatalf("Ping: %v", err)
				}
			} else {
				if _, err := client.PingError(ctx, &pb.PingRequest{}); err == nil {
					t.Fatalf("PingError: expected err got nil")
				}
			}

			ctx = service.ctx
			if ctx == nil {
				t.Error("got nil context")
			}
		})
	}
}
