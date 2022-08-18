package thriftbp

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/avast/retry-go"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/reddit/baseplate.go/breakerbp"
	"github.com/reddit/baseplate.go/ecinterface"
	"github.com/reddit/baseplate.go/errorsbp"
	"github.com/reddit/baseplate.go/internal/gen-go/reddit/baseplate"
	"github.com/reddit/baseplate.go/prometheusbp"
	"github.com/reddit/baseplate.go/retrybp"
	"github.com/reddit/baseplate.go/tracing"
	"github.com/reddit/baseplate.go/transport"
)

// MonitorClientWrappedSlugSuffix is a suffix to be added to the service slug
// arg of MonitorClient function, in order to distinguish from the spans that
// are the raw client calls.
//
// The MonitorClient with this suffix will have span operation names like:
//
//	service-with-retry.endpointName
//
// Which groups all retries of the same client call together,
// while the MonitorClient without this suffix will have span operation names
// like:
//
//	service.endpointName
const MonitorClientWrappedSlugSuffix = transport.WithRetrySlugSuffix

// WithDefaultRetryFilters returns a list of retrybp.Filters by appending the
// given filters to the "default" retry filters:
//
// 1. RetryableErrorFilter - handle errors already provided retryable
// information, this includes clientpool.ErrExhausted
//
// 2. ContextErrorFilter - do not retry on context cancellation/timeout.
func WithDefaultRetryFilters(filters ...retrybp.Filter) []retrybp.Filter {
	return append([]retrybp.Filter{
		retrybp.RetryableErrorFilter,
		retrybp.ContextErrorFilter,
	}, filters...)
}

// DefaultClientMiddlewareArgs is the arg struct for BaseplateDefaultClientMiddlewares.
type DefaultClientMiddlewareArgs struct {
	// ServiceSlug is a short identifier for the thrift service you are creating
	// clients for.  The preferred convention is to take the service's name,
	// remove the 'Service' prefix, if present, and convert from camel case to
	// all lower case, hyphen separated.
	//
	// Examples:
	//
	//     AuthenticationService -> authentication
	//     ImageUploadService -> image-upload
	ServiceSlug string

	// RetryOptions is the list of retry.Options to apply as the defaults for the
	// Retry middleware.
	//
	// This is optional, if it is not set, we will use a single option,
	// retry.Attempts(1).  This sets up the retry middleware but does not
	// automatically retry any requests.  You can set retry behavior per-call by
	// using retrybp.WithOptions.
	RetryOptions []retry.Option

	// Suppress some of the errors returned by the server before sending them to
	// the client span.
	//
	// See MonitorClientArgs.ErrorSpanSuppressor for more details.
	//
	// This is optional. If it's not set IDLExceptionSuppressor will be used.
	ErrorSpanSuppressor errorsbp.Suppressor

	// When BreakerConfig is non-nil,
	// a breakerbp.FailureRatioBreaker will be created for the pool,
	// and its middleware will be set for the pool.
	BreakerConfig *breakerbp.Config

	// The edge context implementation. Optional.
	//
	// If it's not set, the global one from ecinterface.Get will be used instead.
	EdgeContextImpl ecinterface.Interface

	// The name for the server to identify this client,
	// via the "User-Agent" (HeaderUserAgent) THeader.
	//
	// Optional. If this is empty, no "User-Agent" header will be sent.
	ClientName string
}

// BaseplateDefaultClientMiddlewares returns the default client middlewares that
// should be used by a baseplate service.
//
// Currently they are (in order):
//
// 1. ForwardEdgeRequestContext.
//
// 2. SetClientName(clientName)
//
// 3. MonitorClient with MonitorClientWrappedSlugSuffix - This creates the spans
// from the view of the client that group all retries into a single,
// wrapped span.
//
// 4. PrometheusClientMiddleware with MonitorClientWrappedSlugSuffix - This
// creates the prometheus client metrics from the view of the client that group
// all retries into a single operation.
//
// 5. Retry(retryOptions) - If retryOptions is empty/nil, default to only
// retry.Attempts(1), this will not actually retry any calls but your client is
// configured to set retry logic per-call using retrybp.WithOptions.
//
// 6. FailureRatioBreaker - Only if BreakerConfig is non-nil.
//
// 7. MonitorClient - This creates the spans of the raw client calls.
//
// 8. PrometheusClientMiddleware
//
// 9. BaseplateErrorWrapper
//
// 10. SetDeadlineBudget
func BaseplateDefaultClientMiddlewares(args DefaultClientMiddlewareArgs) []thrift.ClientMiddleware {
	if len(args.RetryOptions) == 0 {
		args.RetryOptions = []retry.Option{retry.Attempts(1)}
	}
	middlewares := []thrift.ClientMiddleware{
		ForwardEdgeRequestContext(args.EdgeContextImpl),
		SetClientName(args.ClientName),
		MonitorClient(MonitorClientArgs{
			ServiceSlug:         args.ServiceSlug + MonitorClientWrappedSlugSuffix,
			ErrorSpanSuppressor: args.ErrorSpanSuppressor,
		}),
		PrometheusClientMiddleware(args.ServiceSlug + MonitorClientWrappedSlugSuffix),
		Retry(args.RetryOptions...),
	}
	if args.BreakerConfig != nil {
		middlewares = append(
			middlewares,
			breakerbp.NewFailureRatioBreaker(*args.BreakerConfig).ThriftMiddleware,
		)
	}
	middlewares = append(
		middlewares,
		MonitorClient(MonitorClientArgs{
			ServiceSlug:         args.ServiceSlug,
			ErrorSpanSuppressor: args.ErrorSpanSuppressor,
		}),
		PrometheusClientMiddleware(args.ServiceSlug),
		BaseplateErrorWrapper,
		SetDeadlineBudget,
	)
	return middlewares
}

// MonitorClientArgs are the args to be passed into MonitorClient function.
type MonitorClientArgs struct {
	// The slug string of the service.
	//
	// Note that if this is the MonitorClient before retry,
	// ServiceSlug should also come with MonitorClientWrappedSlugSuffix.
	ServiceSlug string

	// Suppress some of the errors returned by the server before sending them to
	// the client span.
	//
	// Based on Baseplate spec, the errors defined in the server's thrift IDL are
	// not treated as errors, and should be suppressed here. So in most cases
	// that's what should be implemented as the Suppressor here.
	//
	// Note that this suppressor only affects the errors send to the span. It
	// won't affect the errors returned to the caller of the client function.
	//
	// This is optional. If it's not set IDLExceptionSuppressor will be used.
	ErrorSpanSuppressor errorsbp.Suppressor
}

// MonitorClient is a ClientMiddleware that wraps the inner thrift.TClient.Call
// in a thrift client span.
//
// If you are using a thrift ClientPool created by NewBaseplateClientPool,
// this will be included automatically and should not be passed in as a
// ClientMiddleware to NewBaseplateClientPool.
func MonitorClient(args MonitorClientArgs) thrift.ClientMiddleware {
	prefix := args.ServiceSlug + "."
	s := args.ErrorSpanSuppressor
	if s == nil {
		s = IDLExceptionSuppressor
	}
	return func(next thrift.TClient) thrift.TClient {
		return thrift.WrappedTClient{
			Wrapped: func(ctx context.Context, method string, args, result thrift.TStruct) (_ thrift.ResponseMeta, err error) {
				span, ctx := opentracing.StartSpanFromContext(
					ctx,
					prefix+method,
					tracing.SpanTypeOption{
						Type: tracing.SpanTypeClient,
					},
				)
				ctx = CreateThriftContextFromSpan(ctx, tracing.AsSpan(span))
				defer func() {
					span.FinishWithOptions(tracing.FinishOptions{
						Ctx: ctx,
						Err: s.Wrap(getClientError(result, err)),
					}.Convert())
				}()

				return next.Call(ctx, method, args, result)
			},
		}
	}
}

// ForwardEdgeRequestContext forwards the EdgeRequestContext set on the context
// object to the Thrift service being called if one is set.
//
// If you are using a thrift ClientPool created by NewBaseplateClientPool,
// this will be included automatically and should not be passed in as a
// ClientMiddleware to NewBaseplateClientPool.
func ForwardEdgeRequestContext(ecImpl ecinterface.Interface) thrift.ClientMiddleware {
	if ecImpl == nil {
		ecImpl = ecinterface.Get()
	}
	return func(next thrift.TClient) thrift.TClient {
		return thrift.WrappedTClient{
			Wrapped: func(ctx context.Context, method string, args, result thrift.TStruct) (thrift.ResponseMeta, error) {
				ctx = AttachEdgeRequestContext(ctx, ecImpl)
				return next.Call(ctx, method, args, result)
			},
		}
	}
}

// SetDeadlineBudget is the client middleware implementing Phase 1 of Baseplate
// deadline propogation.
func SetDeadlineBudget(next thrift.TClient) thrift.TClient {
	return thrift.WrappedTClient{
		Wrapped: func(ctx context.Context, method string, args, result thrift.TStruct) (thrift.ResponseMeta, error) {
			if ctx.Err() != nil {
				// Deadline already passed, no need to even try
				return thrift.ResponseMeta{}, ctx.Err()
			}

			if deadline, ok := ctx.Deadline(); ok {
				// Round up to the next millisecond.
				// In the scenario that the caller set a 10ms timeout and send the
				// request, by the time we get into this middleware function it's
				// definitely gonna be less than 10ms.
				// If we use round down then we are only gonna send 9 over the wire.
				timeout := time.Until(deadline) + time.Millisecond - 1
				ms := timeout.Milliseconds()
				if ms < 1 {
					// Make sure we give it at least 1ms.
					ms = 1
				}
				value := strconv.FormatInt(ms, 10)
				ctx = AddClientHeader(ctx, transport.HeaderDeadlineBudget, value)
			}

			return next.Call(ctx, method, args, result)
		},
	}
}

// Retry returns a thrift.ClientMiddleware that can be used to automatically
// retry thrift requests.
func Retry(defaults ...retry.Option) thrift.ClientMiddleware {
	return func(next thrift.TClient) thrift.TClient {
		return thrift.WrappedTClient{
			Wrapped: func(ctx context.Context, method string, args, result thrift.TStruct) (thrift.ResponseMeta, error) {
				var lastMeta thrift.ResponseMeta
				return lastMeta, retrybp.Do(
					ctx,
					func() error {
						var err error
						lastMeta, err = next.Call(ctx, method, args, result)
						return getClientError(result, err)
					},
					defaults...,
				)
			},
		}
	}
}

// BaseplateErrorWrapper is a client middleware that calls WrapBaseplateError to
// wrap the error returned by the next client call.
func BaseplateErrorWrapper(next thrift.TClient) thrift.TClient {
	return thrift.WrappedTClient{
		Wrapped: func(ctx context.Context, method string, args, result thrift.TStruct) (thrift.ResponseMeta, error) {
			meta, err := next.Call(ctx, method, args, result)
			return meta, WrapBaseplateError(err)
		},
	}
}

// SetClientName sets the "User-Agent" (HeaderUserAgent) thrift THeader on the
// requests.
//
// If clientName is empty, no "User-Agent" header will be sent.
func SetClientName(clientName string) thrift.ClientMiddleware {
	const header = transport.HeaderUserAgent
	return func(next thrift.TClient) thrift.TClient {
		return thrift.WrappedTClient{
			Wrapped: func(ctx context.Context, method string, args, result thrift.TStruct) (thrift.ResponseMeta, error) {
				if clientName == "" {
					ctx = thrift.UnsetHeader(ctx, header)
				} else {
					ctx = AddClientHeader(ctx, header, clientName)
				}
				return next.Call(ctx, method, args, result)
			},
		}
	}
}

var (
	_ thrift.ClientMiddleware = SetDeadlineBudget
	_ thrift.ClientMiddleware = BaseplateErrorWrapper
)

// PrometheusClientMiddleware returns middleware to track Prometheus metrics
// specific to the Thrift client.
//
// It emits the following prometheus metrics:
//
// * thrift_client_active_requests gauge with labels:
//
//   - thrift_method: the method of the endpoint called
//   - thrift_client_name: an arbitray short string representing the backend the client is connecting to, the remoteServerSlug arg
//
// * thrift_client_latency_seconds histogram with labels above plus:
//
//   - thrift_success: "true" if err == nil, "false" otherwise
//
// * thrift_client_requests_total counter with all labels above plus:
//
//   - thrift_exception_type: the human-readable exception type, e.g.
//     baseplate.Error, etc
//   - thrift_baseplate_status: the numeric status code from a baseplate.Error
//     as a string if present (e.g. 404), or the empty string
//   - thrift_baseplate_status_code: the human-readable status code, e.g.
//     NOT_FOUND, or the empty string
func PrometheusClientMiddleware(remoteServerSlug string) thrift.ClientMiddleware {
	return func(next thrift.TClient) thrift.TClient {
		return thrift.WrappedTClient{
			Wrapped: func(ctx context.Context, method string, args, result thrift.TStruct) (_ thrift.ResponseMeta, err error) {
				start := time.Now()
				activeRequestLabels := prometheus.Labels{
					methodLabel:     method,
					serverSlugLabel: remoteServerSlug,
				}
				clientActiveRequests.With(activeRequestLabels).Inc()

				defer func() {
					var baseplateStatusCode, baseplateStatus string
					finalErr := getClientError(result, err)
					exceptionTypeLabel := stringifyErrorType(finalErr)
					success := prometheusbp.BoolString(finalErr == nil)
					if finalErr != nil {
						var bpErr baseplateErrorCoder
						if errors.As(finalErr, &bpErr) {
							code := bpErr.GetCode()
							baseplateStatusCode = strconv.FormatInt(int64(code), 10)
							if status := baseplate.ErrorCode(code).String(); status != "<UNSET>" {
								baseplateStatus = status
							}
						}
					}

					latencyLabels := prometheus.Labels{
						methodLabel:     method,
						successLabel:    success,
						serverSlugLabel: remoteServerSlug,
					}
					clientLatencyDistribution.With(latencyLabels).Observe(time.Since(start).Seconds())

					totalRequestLabels := prometheus.Labels{
						methodLabel:              method,
						successLabel:             success,
						exceptionLabel:           exceptionTypeLabel,
						baseplateStatusCodeLabel: baseplateStatusCode,
						baseplateStatusLabel:     baseplateStatus,
						serverSlugLabel:          remoteServerSlug,
					}
					clientTotalRequests.With(totalRequestLabels).Inc()
					clientActiveRequests.With(activeRequestLabels).Dec()
				}()

				return next.Call(ctx, method, args, result)
			},
		}
	}
}

// For a endpoint defined in thrift IDL like this:
//
//	service MyService {
//	  FooResponse foo(1: FooRequest request) throws (
//	    1: Exception1 error1,
//	    2: Exception2 error2,
//	  )
//	}
//
// The thrift compiler generated go code for the result TStruct would be like:
//
//	type MyServiceFooResult struct {
//	  Success *FooResponse `thrift:"success,0" db:"success" json:"success,omitempty"`
//	  Error1 *Exception1 `thrift:"error1,1" db:"error1" json:"error1,omitempty"`
//	  Error2 *Exception2 `thrift:"error2,2" db:"error2" json:"error2,omitempty"`
//	}
func getClientError(result thrift.TStruct, err error) error {
	if err != nil {
		return err
	}
	v := reflect.Indirect(reflect.ValueOf(result))
	if v.Kind() != reflect.Struct {
		return nil
	}
	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		if typ.Field(i).Name == "Success" {
			continue
		}
		field := v.Field(i)
		if field.IsZero() {
			continue
		}
		tExc, ok := field.Interface().(thrift.TException)
		if ok && tExc != nil && tExc.TExceptionType() == thrift.TExceptionTypeCompiled {
			return tExc
		}
	}
	return nil
}
