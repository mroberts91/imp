// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Probe describes a periodic health check against a running Proc.
// Field vocabulary mirrors staging/src/k8s.io/api/core/v1/types.go Probe
// (minus gRPC and probe-level terminationGracePeriodSeconds).
type Probe struct {
	Exec      *ExecAction      `json:"exec,omitempty"`
	HTTPGet   *HTTPGetAction   `json:"httpGet,omitempty"`
	TCPSocket *TCPSocketAction `json:"tcpSocket,omitempty"`

	// InitialDelaySeconds is the number of seconds after the process starts
	// before probes begin.
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`
	// TimeoutSeconds is the probe timeout. Defaults to 1. Minimum 1.
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
	// PeriodSeconds is how often to perform the probe. Defaults to 10. Minimum 1.
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`
	// SuccessThreshold is the minimum consecutive successes for the probe to
	// be considered successful after having failed. Defaults to 1. Must be 1
	// for liveness probes.
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
	// FailureThreshold is the minimum consecutive failures for the probe to
	// be considered failed after having succeeded. Defaults to 3.
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
}

// ExecAction runs a command in the Proc's cgroup (exit 0 = success).
type ExecAction struct {
	Command []string `json:"command,omitempty"`
}

// HTTPGetAction performs an HTTP GET against host:port/path.
type HTTPGetAction struct {
	Path        string       `json:"path,omitempty"`
	Port        int32        `json:"port"`
	Host        string       `json:"host,omitempty"`
	Scheme      URIScheme    `json:"scheme,omitempty"`
	HTTPHeaders []HTTPHeader `json:"httpHeaders,omitempty"`
}

// TCPSocketAction opens a TCP connection to host:port.
type TCPSocketAction struct {
	Port int32  `json:"port"`
	Host string `json:"host,omitempty"`
}

// URIScheme is the scheme for HTTPGetAction.
type URIScheme string

const (
	URISchemeHTTP  URIScheme = "HTTP"
	URISchemeHTTPS URIScheme = "HTTPS"
)

// HTTPHeader is a name/value pair for HTTPGetAction.
type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// Ready condition Reasons when a readinessProbe is configured (M3).
const (
	ReadyReasonProbePending = "ProbePending"
	ReadyReasonProbeFailed  = "ProbeFailed"
)
