// Copyright (c) 2024 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package expvar

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/storage/storagetest"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap/zaptest"
)

func TestExpvarExtension(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{
			name:   "good storage",
			status: http.StatusOK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := &Config{
				Port: Port,
			}
			s := newExtension(config, component.TelemetrySettings{
				Logger: zaptest.NewLogger(t),
			})
			require.NoError(t, s.Start(context.Background(), storagetest.NewStorageHost()))
			defer s.Shutdown(context.Background())

			addr := fmt.Sprintf("http://0.0.0.0:%d/", Port)
			client := &http.Client{}
			require.Eventually(t, func() bool {
				r, err := http.NewRequest(http.MethodPost, addr, nil)
				require.NoError(t, err)
				resp, err := client.Do(r)
				require.NoError(t, err)
				defer resp.Body.Close()
				return test.status == resp.StatusCode
			}, 5*time.Second, 100*time.Millisecond)
		})
	}
}
