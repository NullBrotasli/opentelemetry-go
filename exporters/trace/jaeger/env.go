package jaeger

import (
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/api/kv"
	"go.opentelemetry.io/otel/api/kv/value"
)

// Environment variable names
const (
	// The service name.
	envServiceName = "JAEGER_SERVICE_NAME"
	// Whether the exporter is disabled or not. (default false).
	envDisabled = "JAEGER_DISABLED"
	// A comma separated list of name=value tracer-level tags, which get added to all reported spans.
	// The value can also refer to an environment variable using the format ${envVarName:defaultValue}.
	envTags = "JAEGER_TAGS"
	// The HTTP endpoint for sending spans directly to a collector,
	// i.e. http://jaeger-collector:14268/api/traces.
	envEndpoint = "JAEGER_ENDPOINT"
	// Username to send as part of "Basic" authentication to the collector endpoint.
	envUser = "JAEGER_USER"
	// Password to send as part of "Basic" authentication to the collector endpoint.
	envPassword = "JAEGER_PASSWORD"
)

// CollectorEndpointFromEnv return environment variable value of JAEGER_ENDPOINT
func CollectorEndpointFromEnv() string {
	return os.Getenv(envEndpoint)
}

// WithCollectorEndpointOptionFromEnv uses environment variables to set the username and password
// if basic auth is required.
func WithCollectorEndpointOptionFromEnv() CollectorEndpointOption {
	return func(o *CollectorEndpointOptions) {
		if e := os.Getenv(envUser); e != "" {
			o.username = e
		}
		if e := os.Getenv(envPassword); e != "" {
			o.password = os.Getenv(envPassword)
		}
	}
}

// WithDisabledFromEnv uses environment variables and overrides disabled field.
func WithDisabledFromEnv() Option {
	return func(o *options) {
		if e := os.Getenv(envDisabled); e != "" {
			if v, err := strconv.ParseBool(e); err == nil {
				o.Disabled = v
			}
		}
	}
}

// WithProcessFromEnv parse environment variables into jaeger exporter's Process.
func ProcessFromEnv() Process {
	var p Process
	if e := os.Getenv(envServiceName); e != "" {
		p.ServiceName = e
	}
	if e := os.Getenv(envTags); e != "" {
		p.Tags = parseTags(e)
	}

	return p
}

// WithProcessFromEnv uses environment variables and overrides jaeger exporter's Process.
func WithProcessFromEnv() Option {
	return func(o *options) {
		p := ProcessFromEnv()
		if p.ServiceName != "" {
			o.Process.ServiceName = p.ServiceName
		}
		if len(p.Tags) != 0 {
			o.Process.Tags = p.Tags
		}
	}
}

// parseTags parses the given string into a collection of Tags.
// Spec for this value:
// - comma separated list of key=value
// - value can be specified using the notation ${envVar:defaultValue}, where `envVar`
// is an environment variable and `defaultValue` is the value to use in case the env var is not set
func parseTags(sTags string) []kv.KeyValue {
	pairs := strings.Split(sTags, ",")
	tags := make([]kv.KeyValue, 0)
	for _, p := range pairs {
		field := strings.SplitN(p, "=", 2)
		k, v := strings.TrimSpace(field[0]), strings.TrimSpace(field[1])

		if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
			ed := strings.SplitN(v[2:len(v)-1], ":", 2)
			e, d := ed[0], ed[1]
			v = os.Getenv(e)
			if v == "" && d != "" {
				v = d
			}
		}

		tags = append(tags, parseKeyValue(k, v))
	}

	return tags
}

func parseKeyValue(k, v string) kv.KeyValue {
	return kv.KeyValue{
		Key:   kv.Key(k),
		Value: parseValue(v),
	}
}

func parseValue(str string) value.Value {
	if v, err := strconv.ParseInt(str, 10, 64); err == nil {
		return value.Int64(v)
	}
	if v, err := strconv.ParseFloat(str, 64); err == nil {
		return value.Float64(v)
	}
	if v, err := strconv.ParseBool(str); err == nil {
		return value.Bool(v)
	}

	// Fallback
	return value.String(str)
}
