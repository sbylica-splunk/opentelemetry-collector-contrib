// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package windowseventlogreceiver

import (
	"context"
	"encoding/xml"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/confmap/confmaptest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/coreinternal/consumerretry"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/adapter"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator/input/windows"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/windowseventlogreceiver/internal/metadata"
)

func TestDefaultConfig(t *testing.T) {
	factory := newFactoryAdapter()
	cfg := factory.CreateDefaultConfig()
	require.NotNil(t, cfg, "failed to create default config")
	require.NoError(t, componenttest.CheckConfigStruct(cfg))
}

func TestLoadConfig(t *testing.T) {
	cm, err := confmaptest.LoadConf(filepath.Join("testdata", "config.yaml"))
	require.NoError(t, err)
	factory := newFactoryAdapter()
	cfg := factory.CreateDefaultConfig()

	sub, err := cm.Sub(component.NewIDWithName(metadata.Type, "").String())
	require.NoError(t, err)
	require.NoError(t, sub.Unmarshal(cfg))
	assert.Equal(t, createTestConfig(), cfg)
}

func TestCreateWithInvalidInputConfig(t *testing.T) {
	t.Parallel()

	cfg := &WindowsLogConfig{
		BaseConfig: adapter.BaseConfig{},
		InputConfig: func() windows.Config {
			c := windows.NewConfig()
			c.StartAt = "middle"
			return *c
		}(),
	}

	_, err := newFactoryAdapter().CreateLogsReceiver(
		context.Background(),
		receivertest.NewNopSettings(),
		cfg,
		new(consumertest.LogsSink),
	)
	require.Error(t, err, "receiver creation should fail if given invalid input config")
}

func TestReadWindowsEventLogger(t *testing.T) {
	logMessage := "Test log"
	src := "otel-windowseventlogreceiver-test"
	uninstallEventSource, err := assertEventSourceInstallation(t, src)
	defer uninstallEventSource()
	require.NoError(t, err)

	ctx := context.Background()
	factory := newFactoryAdapter()
	createSettings := receivertest.NewNopSettings()
	cfg := createTestConfig()
	sink := new(consumertest.LogsSink)

	receiver, err := factory.CreateLogsReceiver(ctx, createSettings, cfg, sink)
	require.NoError(t, err)

	err = receiver.Start(ctx, componenttest.NewNopHost())
	require.NoError(t, err)
	defer func() {
		require.NoError(t, receiver.Shutdown(ctx))
	}()
	// Start launches nested goroutines, give them a chance to run before logging the test event(s).
	time.Sleep(3 * time.Second)

	logger, err := eventlog.Open(src)
	require.NoError(t, err)
	defer logger.Close()

	err = logger.Info(10, logMessage)
	require.NoError(t, err)

	records := requireExpectedLogRecords(t, sink, src, 1)
	record := records[0]
	body := record.Body().Map().AsRaw()

	require.Equal(t, logMessage, body["message"])

	eventData := body["event_data"]
	eventDataMap, ok := eventData.(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{
		"data": []any{map[string]any{"": "Test log"}},
	}, eventDataMap)

	eventID := body["event_id"]
	require.NotNil(t, eventID)

	eventIDMap, ok := eventID.(map[string]any)
	require.True(t, ok)
	require.Equal(t, int64(10), eventIDMap["id"])
}

func TestReadWindowsEventLoggerRaw(t *testing.T) {
	logMessage := "Test log"
	src := "otel-windowseventlogreceiver-test"
	uninstallEventSource, err := assertEventSourceInstallation(t, src)
	defer uninstallEventSource()
	require.NoError(t, err)

	ctx := context.Background()
	factory := newFactoryAdapter()
	createSettings := receivertest.NewNopSettings()
	cfg := createTestConfig()
	cfg.InputConfig.Raw = true
	sink := new(consumertest.LogsSink)

	receiver, err := factory.CreateLogsReceiver(ctx, createSettings, cfg, sink)
	require.NoError(t, err)

	err = receiver.Start(ctx, componenttest.NewNopHost())
	require.NoError(t, err)
	defer func() {
		require.NoError(t, receiver.Shutdown(ctx))
	}()
	// Start launches nested goroutines, give them a chance to run before logging the test event(s).
	time.Sleep(3 * time.Second)

	logger, err := eventlog.Open(src)
	require.NoError(t, err)
	defer logger.Close()

	err = logger.Info(10, logMessage)
	require.NoError(t, err)

	records := requireExpectedLogRecords(t, sink, src, 1)
	record := records[0]
	body := record.Body().AsString()
	bodyStruct := struct {
		Data string `xml:"EventData>Data"`
	}{}
	err = xml.Unmarshal([]byte(body), &bodyStruct)
	require.NoError(t, err)

	require.Equal(t, logMessage, bodyStruct.Data)
}

func TestExcludeProvider(t *testing.T) {
	logMessage := "Test log"
	excludedSrc := "otel-excluded-windowseventlogreceiver-test"
	notExcludedSrc := "otel-windowseventlogreceiver-test"
	testSources := []string{excludedSrc, notExcludedSrc}

	for _, src := range testSources {
		uninstallEventSource, err := assertEventSourceInstallation(t, src)
		defer uninstallEventSource()
		require.NoError(t, err)
	}

	tests := []struct {
		name string
		raw  bool
	}{
		{
			name: "with_EventXML",
		},
		{
			name: "with_Raw",
			raw:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			factory := newFactoryAdapter()
			createSettings := receivertest.NewNopSettings()
			cfg := createTestConfig()
			cfg.InputConfig.Raw = tt.raw
			cfg.InputConfig.ExcludeProviders = []string{excludedSrc}
			sink := new(consumertest.LogsSink)

			receiver, err := factory.CreateLogsReceiver(ctx, createSettings, cfg, sink)
			require.NoError(t, err)

			err = receiver.Start(ctx, componenttest.NewNopHost())
			require.NoError(t, err)
			defer func() {
				require.NoError(t, receiver.Shutdown(ctx))
			}()
			// Start launches nested goroutines, give them a chance to run before logging the test event(s).
			time.Sleep(3 * time.Second)

			for _, src := range testSources {
				logger, err := eventlog.Open(src)
				require.NoError(t, err)
				defer logger.Close()

				err = logger.Info(10, logMessage)
				require.NoError(t, err)
			}

			records := requireExpectedLogRecords(t, sink, notExcludedSrc, 1)
			assert.NotEmpty(t, records)

			records = filterAllLogRecordsBySource(t, sink, excludedSrc)
			assert.Empty(t, records)
		})
	}
}

func createTestConfig() *WindowsLogConfig {
	return &WindowsLogConfig{
		BaseConfig: adapter.BaseConfig{
			Operators:      []operator.Config{},
			RetryOnFailure: consumerretry.NewDefaultConfig(),
		},
		InputConfig: func() windows.Config {
			c := windows.NewConfig()
			c.Channel = "application"
			c.StartAt = "end"
			return *c
		}(),
	}
}

// assertEventSourceInstallation installs an event source and verifies that the registry key was created.
// It returns a function that can be used to uninstall the event source, that function is never nil
func assertEventSourceInstallation(t *testing.T, src string) (uninstallEventSource func(), err error) {
	err = eventlog.InstallAsEventCreate(src, eventlog.Info|eventlog.Warning|eventlog.Error)
	uninstallEventSource = func() {
		assert.NoError(t, eventlog.Remove(src))
	}
	assert.NoError(t, err)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		rk, err := registry.OpenKey(registry.LOCAL_MACHINE, "SYSTEM\\CurrentControlSet\\Services\\EventLog\\Application\\"+src, registry.QUERY_VALUE)
		assert.NoError(c, err)
		defer rk.Close()
		_, _, err = rk.GetIntegerValue("TypesSupported")
		assert.NoError(c, err)
	}, 10*time.Second, 250*time.Millisecond)

	return
}

func requireExpectedLogRecords(t *testing.T, sink *consumertest.LogsSink, expectedEventSrc string, expectedEventCount int) []plog.LogRecord {
	var actualLogRecords []plog.LogRecord

	// logs sometimes take a while to be written, so a substantial wait buffer is needed
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		actualLogRecords = filterAllLogRecordsBySource(t, sink, expectedEventSrc)
		assert.Len(c, actualLogRecords, expectedEventCount)
	}, 10*time.Second, 250*time.Millisecond)

	return actualLogRecords
}

func filterAllLogRecordsBySource(t *testing.T, sink *consumertest.LogsSink, src string) (filteredLogRecords []plog.LogRecord) {
	for _, logs := range sink.AllLogs() {
		resourceLogsSlice := logs.ResourceLogs()
		for i := 0; i < resourceLogsSlice.Len(); i++ {
			resourceLogs := resourceLogsSlice.At(i)
			scopeLogsSlice := resourceLogs.ScopeLogs()
			for j := 0; j < scopeLogsSlice.Len(); j++ {
				logRecords := scopeLogsSlice.At(j).LogRecords()
				for k := 0; k < logRecords.Len(); k++ {
					logRecord := logRecords.At(k)
					if extractEventSourceFromLogRecord(t, logRecord) == src {
						filteredLogRecords = append(filteredLogRecords, logRecord)
					}
				}
			}
		}
	}

	return
}

func extractEventSourceFromLogRecord(t *testing.T, logRecord plog.LogRecord) string {
	eventMap := logRecord.Body().Map()
	if !reflect.DeepEqual(eventMap, pcommon.Map{}) {
		eventProviderMap := eventMap.AsRaw()["provider"]
		if providerMap, ok := eventProviderMap.(map[string]any); ok {
			return providerMap["name"].(string)
		}
		require.Fail(t, "Failed to extract event source from log record")
	}

	// This is a raw event log record. Extract the event source from the XML body string.
	bodyString := logRecord.Body().AsString()
	var eventXML windows.EventXML
	require.NoError(t, xml.Unmarshal([]byte(bodyString), &eventXML))
	return eventXML.Provider.Name
}
