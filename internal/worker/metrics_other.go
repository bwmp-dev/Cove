//go:build !linux

package worker

import "context"

func inspectMetrics(context.Context, string) *NodeMetrics { return nil }
