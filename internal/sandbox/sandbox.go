// Package sandbox starts one fresh, unprivileged container per run.
package sandbox

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrTimeout is returned when a run exceeds Spec.Timeout.
var ErrTimeout = errors.New("sandbox: run exceeded timeout")

// Mount binds a host directory into the container.
type Mount struct {
	HostPath, ContainerPath string
	ReadOnly                bool
}

// Spec describes one sandbox run. Name identifies the container so it
// can be removed on timeout; Env must never hold real credentials.
type Spec struct {
	Name, Image, Network string
	Mounts               []Mount
	Env                  map[string]string
	Stdin                []byte
	Timeout              time.Duration
}

// Provider runs a Spec to completion, streaming the container's stdout
// and stderr. It returns ErrTimeout on timeout and the context error on
// cancellation; in both cases the container is gone afterwards.
type Provider interface {
	Run(ctx context.Context, s Spec, stdout, stderr io.Writer) error
}
