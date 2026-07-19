//go:build tools

// Package tools pins build-time tool dependencies so `go run` resolves them
// from go.mod instead of floating versions.
package tools

import (
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen"
)
