// Copyright (c) 2019 The Jaeger Authors.
// Copyright (c) 2017 Uber Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package spanstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/olivere/elastic/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/dbmodel"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/mocks"
	"github.com/jaegertracing/jaeger/internal/testutils"
)

const (
	defaultMaxDocCount = 10_000
	testingTraceId     = "testing-id"
)

var exampleESSpan = []byte(
	`{
	   "traceID": "1",
	   "parentSpanID": "2",
	   "spanID": "3",
	   "flags": 0,
	   "operationName": "op",
	   "references": [],
	   "startTime": 812965625,
	   "duration": 3290114992,
	   "tags": [
	      {
		 "key": "tag",
		 "value": "1965806585",
		 "type": "int64"
	      }
	   ],
	   "logs": [
	      {
		 "timestamp": 812966073,
		 "fields": [
		    {
		       "key": "logtag",
		       "value": "helloworld",
		       "type": "string"
		    }
		 ]
	      }
	   ],
	   "process": {
	      "serviceName": "serv",
	      "tags": [
		 {
		    "key": "processtag",
		    "value": "false",
		    "type": "bool"
		 }
	      ]
	   }
	}`)

type spanReaderTest struct {
	client      *mocks.Client
	logger      *zap.Logger
	logBuffer   *testutils.Buffer
	traceBuffer *tracetest.InMemoryExporter
	reader      *SpanReader
}

func tracerProvider(t *testing.T) (trace.TracerProvider, *tracetest.InMemoryExporter, func()) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exporter),
	)
	closer := func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	}
	return tp, exporter, closer
}

func withSpanReader(t *testing.T, fn func(r *spanReaderTest)) {
	client := &mocks.Client{}
	tracer, exp, closer := tracerProvider(t)
	defer closer()
	logger, logBuffer := testutils.NewLogger()
	r := &spanReaderTest{
		client:      client,
		logger:      logger,
		logBuffer:   logBuffer,
		traceBuffer: exp,
		reader: NewSpanReader(SpanReaderParams{
			Client:            func() es.Client { return client },
			Logger:            zap.NewNop(),
			Tracer:            tracer.Tracer("test"),
			MaxSpanAge:        0,
			TagDotReplacement: "@",
			MaxDocCount:       defaultMaxDocCount,
		}),
	}
	fn(r)
}

func withArchiveSpanReader(t *testing.T, readAlias bool, readAliasSuffix string, fn func(r *spanReaderTest)) {
	client := &mocks.Client{}
	tracer, exp, closer := tracerProvider(t)
	defer closer()
	logger, logBuffer := testutils.NewLogger()
	r := &spanReaderTest{
		client:      client,
		logger:      logger,
		logBuffer:   logBuffer,
		traceBuffer: exp,
		reader: NewSpanReader(SpanReaderParams{
			Client:              func() es.Client { return client },
			Logger:              zap.NewNop(),
			Tracer:              tracer.Tracer("test"),
			MaxSpanAge:          0,
			TagDotReplacement:   "@",
			ReadAliasSuffix:     readAliasSuffix,
			UseReadWriteAliases: readAlias,
		}),
	}
	fn(r)
}

func TestNewSpanReader(t *testing.T) {
	tests := []struct {
		name       string
		params     SpanReaderParams
		maxSpanAge time.Duration
	}{
		{
			name: "no rollover",
			params: SpanReaderParams{
				MaxSpanAge: time.Hour * 72,
			},
			maxSpanAge: time.Hour * 72,
		},
		{
			name: "rollover enabled",
			params: SpanReaderParams{
				MaxSpanAge:          time.Hour * 72,
				UseReadWriteAliases: true,
			},
			maxSpanAge: time.Hour * 24 * 365 * 50,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := test.params
			params.Logger = zaptest.NewLogger(t)
			reader := NewSpanReader(params)
			require.NotNil(t, reader)
			assert.Equal(t, test.maxSpanAge, reader.maxSpanAge)
		})
	}
}

func TestSpanReaderIndices(t *testing.T) {
	client := &mocks.Client{}
	clientFn := func() es.Client { return client }
	date := time.Date(2019, 10, 10, 5, 0, 0, 0, time.UTC)

	spanDataLayout := "2006-01-02-15"
	serviceDataLayout := "2006-01-02"
	spanDataLayoutFormat := date.UTC().Format(spanDataLayout)
	serviceDataLayoutFormat := date.UTC().Format(serviceDataLayout)

	logger, _ := testutils.NewLogger()
	tracer, _, closer := tracerProvider(t)
	defer closer()

	spanIndexOpts := config.IndexOptions{DateLayout: spanDataLayout}
	serviceIndexOpts := config.IndexOptions{DateLayout: serviceDataLayout}

	testCases := []struct {
		indices []string
		params  SpanReaderParams
	}{
		{
			params: SpanReaderParams{
				SpanIndex:    spanIndexOpts,
				ServiceIndex: serviceIndexOpts,
			},
			indices: []string{spanIndexBaseName + spanDataLayoutFormat, serviceIndexBaseName + serviceDataLayoutFormat},
		},
		{
			params: SpanReaderParams{
				UseReadWriteAliases: true,
			},
			indices: []string{spanIndexBaseName + "read", serviceIndexBaseName + "read"},
		},
		{
			params: SpanReaderParams{
				ReadAliasSuffix: "archive", // ignored because ReadWriteAliases is false
			},
			indices: []string{spanIndexBaseName, serviceIndexBaseName},
		},
		{
			params: SpanReaderParams{
				SpanIndex:    spanIndexOpts,
				ServiceIndex: serviceIndexOpts,
				IndexPrefix:  "foo:",
			},
			indices: []string{"foo:" + config.IndexPrefixSeparator + spanIndexBaseName + spanDataLayoutFormat, "foo:" + config.IndexPrefixSeparator + serviceIndexBaseName + serviceDataLayoutFormat},
		},
		{
			params: SpanReaderParams{
				SpanIndex: spanIndexOpts, ServiceIndex: serviceIndexOpts, IndexPrefix: "foo:", UseReadWriteAliases: true,
			},
			indices: []string{"foo:-" + spanIndexBaseName + "read", "foo:-" + serviceIndexBaseName + "read"},
		},
		{
			params: SpanReaderParams{
				ReadAliasSuffix:     "archive",
				UseReadWriteAliases: true,
			},
			indices: []string{spanIndexBaseName + "archive", serviceIndexBaseName + "archive"},
		},
		{
			params: SpanReaderParams{
				SpanIndex: spanIndexOpts, ServiceIndex: serviceIndexOpts, IndexPrefix: "foo:", UseReadWriteAliases: true, ReadAliasSuffix: "archive",
			},
			indices: []string{"foo:" + config.IndexPrefixSeparator + spanIndexBaseName + "archive", "foo:" + config.IndexPrefixSeparator + serviceIndexBaseName + "archive"},
		},
		{
			params: SpanReaderParams{
				SpanIndex:          spanIndexOpts,
				ServiceIndex:       serviceIndexOpts,
				RemoteReadClusters: []string{"cluster_one", "cluster_two"},
			},
			indices: []string{
				spanIndexBaseName + spanDataLayoutFormat,
				"cluster_one:" + spanIndexBaseName + spanDataLayoutFormat,
				"cluster_two:" + spanIndexBaseName + spanDataLayoutFormat,
				serviceIndexBaseName + serviceDataLayoutFormat,
				"cluster_one:" + serviceIndexBaseName + serviceDataLayoutFormat,
				"cluster_two:" + serviceIndexBaseName + serviceDataLayoutFormat,
			},
		},
		{
			params: SpanReaderParams{
				UseReadWriteAliases: true, ReadAliasSuffix: "archive", RemoteReadClusters: []string{"cluster_one", "cluster_two"},
			},
			indices: []string{
				spanIndexBaseName + "archive",
				"cluster_one:" + spanIndexBaseName + "archive",
				"cluster_two:" + spanIndexBaseName + "archive",
				serviceIndexBaseName + "archive",
				"cluster_one:" + serviceIndexBaseName + "archive",
				"cluster_two:" + serviceIndexBaseName + "archive",
			},
		},
		{
			params: SpanReaderParams{
				UseReadWriteAliases: true, RemoteReadClusters: []string{"cluster_one", "cluster_two"},
			},
			indices: []string{
				spanIndexBaseName + "read",
				"cluster_one:" + spanIndexBaseName + "read",
				"cluster_two:" + spanIndexBaseName + "read",
				serviceIndexBaseName + "read",
				"cluster_one:" + serviceIndexBaseName + "read",
				"cluster_two:" + serviceIndexBaseName + "read",
			},
		},
	}
	for _, testCase := range testCases {
		testCase.params.Client = clientFn
		testCase.params.Logger = logger
		testCase.params.Tracer = tracer.Tracer("test")
		r := NewSpanReader(testCase.params)

		actualSpan := r.timeRangeIndices(r.spanIndexPrefix, r.spanIndex.DateLayout, date, date, -1*time.Hour)
		actualService := r.timeRangeIndices(r.serviceIndexPrefix, r.serviceIndex.DateLayout, date, date, -24*time.Hour)
		assert.Equal(t, testCase.indices, append(actualSpan, actualService...))
	}
}

func TestSpanReader_GetTrace(t *testing.T) {
	withSpanReader(t, func(r *spanReaderTest) {
		hits := make([]*elastic.SearchHit, 1)
		hits[0] = &elastic.SearchHit{
			Source: exampleESSpan,
		}
		searchHits := &elastic.SearchHits{Hits: hits}

		mockSearchService(r).Return(&elastic.SearchResult{Hits: searchHits}, nil)
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{
					{Hits: searchHits},
				},
			}, nil)
		query := []dbmodel.TraceID{dbmodel.TraceID(testingTraceId)}
		trace, err := r.reader.GetTraces(context.Background(), query)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.NoError(t, err)
		require.NotNil(t, trace)
		assert.Len(t, trace, 1)
		expectedSpans, err := r.reader.collectSpans(hits)
		require.NoError(t, err)

		require.Len(t, trace[0].Spans, 1)
		assert.Equal(t, trace[0].Spans[0], expectedSpans[0])
	})
}

func newSearchRequest(fn *elastic.SearchSource) *elastic.SearchRequest {
	return elastic.NewSearchRequest().
		IgnoreUnavailable(true).
		Source(fn)
}

func TestSpanReader_multiRead_followUp_query(t *testing.T) {
	withSpanReader(t, func(r *spanReaderTest) {
		traceID1 := dbmodel.TraceID(testingTraceId + "1")
		traceID2 := dbmodel.TraceID(testingTraceId + "2")
		date := time.Date(2019, 10, 10, 5, 0, 0, 0, time.UTC)
		spanID1 := dbmodel.Span{
			SpanID:    "0",
			TraceID:   traceID1,
			StartTime: model.TimeAsEpochMicroseconds(date),
			Tags:      []dbmodel.KeyValue{},
			Process: dbmodel.Process{
				Tags: []dbmodel.KeyValue{},
			},
		}
		spanBytesID1, err := json.Marshal(spanID1)
		require.NoError(t, err)
		spanID2 := dbmodel.Span{
			SpanID:    "0",
			TraceID:   traceID2,
			StartTime: model.TimeAsEpochMicroseconds(date),
			Tags:      []dbmodel.KeyValue{},
			Process: dbmodel.Process{
				Tags: []dbmodel.KeyValue{},
			},
		}
		spanBytesID2, err := json.Marshal(spanID2)
		require.NoError(t, err)

		traceID1Query := elastic.NewTermQuery(traceIDField, string(traceID1))
		id1Query := elastic.NewBoolQuery().Must(traceID1Query)
		id1Search := newSearchRequest(r.reader.sourceFn(id1Query, model.TimeAsEpochMicroseconds(date.Add(-time.Hour))).TrackTotalHits(true))
		traceID2Query := elastic.NewTermQuery(traceIDField, string(traceID2))
		id2Query := elastic.NewBoolQuery().Must(traceID2Query)
		id2Search := newSearchRequest(r.reader.sourceFn(id2Query, model.TimeAsEpochMicroseconds(date.Add(-time.Hour))).TrackTotalHits(true))
		id1SearchSpanTime := newSearchRequest(r.reader.sourceFn(id1Query, spanID1.StartTime).TrackTotalHits(true))

		multiSearchService := &mocks.MultiSearchService{}
		firstMultiSearch := &mocks.MultiSearchService{}
		secondMultiSearch := &mocks.MultiSearchService{}
		multiSearchService.On("Add", mock.MatchedBy(func(searches []*elastic.SearchRequest) bool {
			return len(searches) == 2 &&
				reflect.DeepEqual(searches[0], id1Search) &&
				reflect.DeepEqual(searches[1], id2Search)
		})).Return(firstMultiSearch).Once()

		multiSearchService.On("Add", mock.MatchedBy(func(searches []*elastic.SearchRequest) bool {
			return len(searches) == 1 &&
				reflect.DeepEqual(searches[0], id1SearchSpanTime)
		})).Return(secondMultiSearch).Once()

		firstMultiSearch.On("Index", mock.AnythingOfType("[]string")).Return(firstMultiSearch)
		secondMultiSearch.On("Index", mock.AnythingOfType("[]string")).Return(secondMultiSearch)
		r.client.On("MultiSearch").Return(multiSearchService)

		fistMultiSearchMock := firstMultiSearch.On("Do", mock.Anything)
		secondMultiSearchMock := secondMultiSearch.On("Do", mock.Anything)

		// set TotalHits to two to trigger the follow up query
		// the client will return only one span therefore the implementation
		// triggers follow up query for the same traceID with the timestamp of the last span
		searchHitsID1 := &elastic.SearchHits{Hits: []*elastic.SearchHit{
			{Source: spanBytesID1},
		}, TotalHits: &elastic.TotalHits{
			Value:    2,
			Relation: "eq",
		}}
		fistMultiSearchMock.
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{
					{Hits: searchHitsID1},
				},
			}, nil)

		searchHitsID2 := &elastic.SearchHits{Hits: []*elastic.SearchHit{
			{Source: spanBytesID2},
		}, TotalHits: &elastic.TotalHits{
			Value:    1,
			Relation: "eq",
		}}
		secondMultiSearchMock.
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{
					{Hits: searchHitsID2},
				},
			}, nil)

		traces, err := r.reader.multiRead(context.Background(), []dbmodel.TraceID{traceID1, traceID2}, date, date)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.NoError(t, err)
		require.NotNil(t, traces)
		require.Len(t, traces, 2)

		for i, s := range []dbmodel.Span{spanID1, spanID2} {
			actual := traces[i].Spans[0]
			actualData, err := json.Marshal(actual)
			require.NoError(t, err)
			expectedData, err := json.Marshal(s)
			require.NoError(t, err)
			assert.Equal(t, string(expectedData), string(actualData))
		}
	})
}

func TestSpanReader_SearchAfter(t *testing.T) {
	withSpanReader(t, func(r *spanReaderTest) {
		var hits []*elastic.SearchHit

		for i := 0; i < 10000; i++ {
			hit := &elastic.SearchHit{Source: exampleESSpan}
			hits = append(hits, hit)
		}

		totalHits := &elastic.TotalHits{
			Value:    int64(10040),
			Relation: "eq",
		}

		searchHits := &elastic.SearchHits{Hits: hits, TotalHits: totalHits}

		mockSearchService(r).Return(&elastic.SearchResult{Hits: searchHits}, nil)
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{
					{Hits: searchHits},
				},
			}, nil).Times(2)

		query := []dbmodel.TraceID{dbmodel.TraceID("testing-id")}
		trace, err := r.reader.GetTraces(context.Background(), query)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.NoError(t, err)
		require.NotNil(t, trace)
		assert.Len(t, trace, 1)
		expectedSpans, err := r.reader.collectSpans(hits)
		require.NoError(t, err)

		assert.Equal(t, trace[0].Spans[0], expectedSpans[0])
	})
}

func TestSpanReader_GetTraceQueryError(t *testing.T) {
	withSpanReader(t, func(r *spanReaderTest) {
		mockSearchService(r).
			Return(nil, errors.New("query error occurred"))
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{},
			}, nil)
		query := []dbmodel.TraceID{dbmodel.TraceID("testing-id")}
		trace, err := r.reader.GetTraces(context.Background(), query)
		require.NoError(t, err)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.Empty(t, trace)
	})
}

func TestSpanReader_GetTraceNilHits(t *testing.T) {
	withSpanReader(t, func(r *spanReaderTest) {
		var hits []*elastic.SearchHit
		searchHits := &elastic.SearchHits{Hits: hits}

		mockSearchService(r).Return(&elastic.SearchResult{Hits: searchHits}, nil)
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{
					{Hits: nil},
				},
			}, nil)

		query := []dbmodel.TraceID{dbmodel.TraceID(testingTraceId)}
		trace, err := r.reader.GetTraces(context.Background(), query)
		require.NoError(t, err)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.Empty(t, trace)
	})
}

func TestSpanReader_GetTraceInvalidSpanError(t *testing.T) {
	withSpanReader(t, func(r *spanReaderTest) {
		data := []byte(`{"TraceID": "123"asdf fadsg}`)
		hits := make([]*elastic.SearchHit, 1)
		hits[0] = &elastic.SearchHit{
			Source: data,
		}
		searchHits := &elastic.SearchHits{Hits: hits}

		mockSearchService(r).Return(&elastic.SearchResult{Hits: searchHits}, nil)
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{
					{Hits: searchHits},
				},
			}, nil)

		query := []dbmodel.TraceID{dbmodel.TraceID(testingTraceId)}
		trace, err := r.reader.GetTraces(context.Background(), query)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.Error(t, err, "invalid span")
		require.Nil(t, trace)
	})
}

func TestSpanReader_esJSONtoJSONSpanModel(t *testing.T) {
	withSpanReader(t, func(r *spanReaderTest) {
		jsonPayload := exampleESSpan

		esSpanRaw := &elastic.SearchHit{
			Source: jsonPayload,
		}

		span, err := r.reader.unmarshalJSONSpan(esSpanRaw)
		require.NoError(t, err)

		var expectedSpan dbmodel.Span
		require.NoError(t, json.Unmarshal(exampleESSpan, &expectedSpan))
		assert.Equal(t, expectedSpan, span)
	})
}

func TestSpanReader_esJSONtoJSONSpanModelError(t *testing.T) {
	withSpanReader(t, func(r *spanReaderTest) {
		data := []byte(`{"TraceID": "123"asdf fadsg}`)
		jsonPayload := data

		esSpanRaw := &elastic.SearchHit{
			Source: jsonPayload,
		}

		_, err := r.reader.unmarshalJSONSpan(esSpanRaw)
		require.Error(t, err)
	})
}

func TestSpanReaderFindIndices(t *testing.T) {
	today := time.Date(1995, time.April, 21, 4, 12, 19, 95, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	twoDaysAgo := today.AddDate(0, 0, -2)
	dateLayout := "2006-01-02"

	testCases := []struct {
		startTime time.Time
		endTime   time.Time
		expected  []string
	}{
		{
			startTime: today.Add(-time.Millisecond),
			endTime:   today,
			expected: []string{
				indexWithDate(spanIndexBaseName, dateLayout, today),
			},
		},
		{
			startTime: today.Add(-13 * time.Hour),
			endTime:   today,
			expected: []string{
				indexWithDate(spanIndexBaseName, dateLayout, today),
				indexWithDate(spanIndexBaseName, dateLayout, yesterday),
			},
		},
		{
			startTime: today.Add(-48 * time.Hour),
			endTime:   today,
			expected: []string{
				indexWithDate(spanIndexBaseName, dateLayout, today),
				indexWithDate(spanIndexBaseName, dateLayout, yesterday),
				indexWithDate(spanIndexBaseName, dateLayout, twoDaysAgo),
			},
		},
	}
	withSpanReader(t, func(r *spanReaderTest) {
		for _, testCase := range testCases {
			actual := r.reader.timeRangeIndices(spanIndexBaseName, dateLayout, testCase.startTime, testCase.endTime, -24*time.Hour)
			assert.Equal(t, testCase.expected, actual)
		}
	})
}

func TestSpanReader_indexWithDate(t *testing.T) {
	withSpanReader(t, func(_ *spanReaderTest) {
		actual := indexWithDate(spanIndexBaseName, "2006-01-02", time.Date(1995, time.April, 21, 4, 21, 19, 95, time.UTC))
		assert.Equal(t, "jaeger-span-1995-04-21", actual)
	})
}

func testGet(typ string, t *testing.T) {
	goodAggregations := make(map[string]json.RawMessage)
	rawMessage := []byte(`{"buckets": [{"key": "123","doc_count": 16}]}`)
	goodAggregations[typ] = rawMessage

	badAggregations := make(map[string]json.RawMessage)
	badRawMessage := []byte(`{"buckets": [{bad json]}asdf`)
	badAggregations[typ] = badRawMessage

	testCases := []struct {
		caption        string
		searchResult   *elastic.SearchResult
		searchError    error
		expectedError  func() string
		expectedOutput map[string]any
	}{
		{
			caption:      typ + " full behavior",
			searchResult: &elastic.SearchResult{Aggregations: elastic.Aggregations(goodAggregations)},
			expectedOutput: map[string]any{
				operationsAggregation: []dbmodel.Operation{{Name: "123"}},
				traceIDAggregation:    []dbmodel.TraceID{"123"},
				"default":             []string{"123"},
			},
			expectedError: func() string {
				return ""
			},
		},
		{
			caption:     typ + " search error",
			searchError: errors.New("Search failure"),
			expectedError: func() string {
				if typ == operationsAggregation {
					return "search operations failed: Search failure"
				}
				return "search services failed: Search failure"
			},
		},
		{
			caption:      typ + " search error",
			searchResult: &elastic.SearchResult{Aggregations: elastic.Aggregations(badAggregations)},
			expectedError: func() string {
				return "could not find aggregation of " + typ
			},
		},
	}

	for _, tc := range testCases {
		testCase := tc
		t.Run(testCase.caption, func(t *testing.T) {
			withSpanReader(t, func(r *spanReaderTest) {
				mockSearchService(r).Return(testCase.searchResult, testCase.searchError)
				actual, err := returnSearchFunc(typ, r)
				if testCase.expectedError() != "" {
					require.EqualError(t, err, testCase.expectedError())
					assert.Nil(t, actual)
				} else if expectedOutput, ok := testCase.expectedOutput[typ]; ok {
					assert.Equal(t, expectedOutput, actual)
				} else {
					assert.Equal(t, testCase.expectedOutput["default"], actual)
				}
			})
		})
	}
}

func returnSearchFunc(typ string, r *spanReaderTest) (any, error) {
	switch typ {
	case servicesAggregation:
		return r.reader.GetServices(context.Background())
	case operationsAggregation:
		return r.reader.GetOperations(
			context.Background(),
			dbmodel.OperationQueryParameters{ServiceName: "someService"},
		)
	case traceIDAggregation:
		return r.reader.findTraceIDs(context.Background(), dbmodel.TraceQueryParameters{})
	default:
		return nil, errors.New("Specify services, operations, traceIDs only")
	}
}

func TestSpanReader_bucketToStringArray(t *testing.T) {
	withSpanReader(t, func(_ *spanReaderTest) {
		buckets := make([]*elastic.AggregationBucketKeyItem, 3)
		buckets[0] = &elastic.AggregationBucketKeyItem{Key: "hello"}
		buckets[1] = &elastic.AggregationBucketKeyItem{Key: "world"}
		buckets[2] = &elastic.AggregationBucketKeyItem{Key: "2"}

		actual, err := bucketToStringArray[string](buckets)
		require.NoError(t, err)

		assert.Equal(t, []string{"hello", "world", "2"}, actual)
	})
}

func TestSpanReader_bucketToStringArrayError(t *testing.T) {
	withSpanReader(t, func(_ *spanReaderTest) {
		buckets := make([]*elastic.AggregationBucketKeyItem, 3)
		buckets[0] = &elastic.AggregationBucketKeyItem{Key: "hello"}
		buckets[1] = &elastic.AggregationBucketKeyItem{Key: "world"}
		buckets[2] = &elastic.AggregationBucketKeyItem{Key: 2}

		_, err := bucketToStringArray[string](buckets)
		require.EqualError(t, err, "non-string key found in aggregation")
	})
}

func TestSpanReader_FindTraces(t *testing.T) {
	goodAggregations := make(map[string]json.RawMessage)
	rawMessage := []byte(`{"buckets": [{"key": "1","doc_count": 16},{"key": "2","doc_count": 16},{"key": "3","doc_count": 16}]}`)
	goodAggregations[traceIDAggregation] = rawMessage

	hits := make([]*elastic.SearchHit, 1)
	hits[0] = &elastic.SearchHit{
		Source: exampleESSpan,
	}
	searchHits := &elastic.SearchHits{Hits: hits}

	withSpanReader(t, func(r *spanReaderTest) {
		// find trace IDs
		mockSearchService(r).
			Return(&elastic.SearchResult{Aggregations: elastic.Aggregations(goodAggregations), Hits: searchHits}, nil)
		// bulk read traces
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{
					{Hits: searchHits},
					{Hits: searchHits},
				},
			}, nil)

		traceQuery := dbmodel.TraceQueryParameters{
			ServiceName: serviceName,
			Tags: map[string]string{
				"hello": "world",
			},
			StartTimeMin: time.Now().Add(-1 * time.Hour),
			StartTimeMax: time.Now(),
			NumTraces:    1,
		}

		traces, err := r.reader.FindTraces(context.Background(), traceQuery)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.NoError(t, err)
		assert.Len(t, traces, 1)

		trace := traces[0]
		expectedSpans, err := r.reader.collectSpans(hits)
		require.NoError(t, err)

		require.Len(t, trace.Spans, 2)
		assert.Equal(t, trace.Spans[0], expectedSpans[0])
	})
}

func TestSpanReader_FindTracesInvalidQuery(t *testing.T) {
	goodAggregations := make(map[string]json.RawMessage)
	rawMessage := []byte(`{"buckets": [{"key": "1","doc_count": 16},{"key": "2","doc_count": 16},{"key": "3","doc_count": 16}]}`)
	goodAggregations[traceIDAggregation] = rawMessage

	hits := make([]*elastic.SearchHit, 1)
	hits[0] = &elastic.SearchHit{
		Source: exampleESSpan,
	}
	searchHits := &elastic.SearchHits{Hits: hits}

	withSpanReader(t, func(r *spanReaderTest) {
		mockSearchService(r).
			Return(&elastic.SearchResult{Aggregations: elastic.Aggregations(goodAggregations), Hits: searchHits}, nil)
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{
					{Hits: searchHits},
					{Hits: searchHits},
				},
			}, nil)

		traceQuery := dbmodel.TraceQueryParameters{
			ServiceName: "",
			Tags: map[string]string{
				"hello": "world",
			},
			StartTimeMin: time.Now().Add(-1 * time.Hour),
			StartTimeMax: time.Now(),
		}

		traces, err := r.reader.FindTraces(context.Background(), traceQuery)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.Error(t, err)
		assert.Nil(t, traces)
	})
}

func TestSpanReader_FindTracesAggregationFailure(t *testing.T) {
	goodAggregations := make(map[string]json.RawMessage)

	hits := make([]*elastic.SearchHit, 1)
	hits[0] = &elastic.SearchHit{
		Source: exampleESSpan,
	}
	searchHits := &elastic.SearchHits{Hits: hits}

	withSpanReader(t, func(r *spanReaderTest) {
		mockSearchService(r).
			Return(&elastic.SearchResult{Aggregations: elastic.Aggregations(goodAggregations), Hits: searchHits}, nil)
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{},
			}, nil)

		traceQuery := dbmodel.TraceQueryParameters{
			ServiceName: serviceName,
			Tags: map[string]string{
				"hello": "world",
			},
			StartTimeMin: time.Now().Add(-1 * time.Hour),
			StartTimeMax: time.Now(),
		}

		traces, err := r.reader.FindTraces(context.Background(), traceQuery)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.Error(t, err)
		assert.Nil(t, traces)
	})
}

func TestSpanReader_FindTracesNoTraceIDs(t *testing.T) {
	goodAggregations := make(map[string]json.RawMessage)
	rawMessage := []byte(`{"buckets": []}`)
	goodAggregations[traceIDAggregation] = rawMessage

	hits := make([]*elastic.SearchHit, 1)
	hits[0] = &elastic.SearchHit{
		Source: exampleESSpan,
	}
	searchHits := &elastic.SearchHits{Hits: hits}

	withSpanReader(t, func(r *spanReaderTest) {
		mockSearchService(r).
			Return(&elastic.SearchResult{Aggregations: elastic.Aggregations(goodAggregations), Hits: searchHits}, nil)
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{},
			}, nil)

		traceQuery := dbmodel.TraceQueryParameters{
			ServiceName: serviceName,
			Tags: map[string]string{
				"hello": "world",
			},
			StartTimeMin: time.Now().Add(-1 * time.Hour),
			StartTimeMax: time.Now(),
		}

		traces, err := r.reader.FindTraces(context.Background(), traceQuery)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.NoError(t, err)
		assert.Empty(t, traces)
	})
}

func TestSpanReader_FindTracesReadTraceFailure(t *testing.T) {
	goodAggregations := make(map[string]json.RawMessage)
	rawMessage := []byte(`{"buckets": [{"key": "1","doc_count": 16},{"key": "2","doc_count": 16}]}`)
	goodAggregations[traceIDAggregation] = rawMessage

	badSpan := []byte(`{"TraceID": "123"asjlgajdfhilqghi[adfvca} bad json`)
	hits := make([]*elastic.SearchHit, 1)
	hits[0] = &elastic.SearchHit{
		Source: badSpan,
	}
	searchHits := &elastic.SearchHits{Hits: hits}

	withSpanReader(t, func(r *spanReaderTest) {
		mockSearchService(r).
			Return(&elastic.SearchResult{Aggregations: elastic.Aggregations(goodAggregations), Hits: searchHits}, nil)
		mockMultiSearchService(r).
			Return(nil, errors.New("read error"))

		traceQuery := dbmodel.TraceQueryParameters{
			ServiceName: serviceName,
			Tags: map[string]string{
				"hello": "world",
			},
			StartTimeMin: time.Now().Add(-1 * time.Hour),
			StartTimeMax: time.Now(),
		}

		traces, err := r.reader.FindTraces(context.Background(), traceQuery)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.EqualError(t, err, "read error")
		assert.Empty(t, traces)
	})
}

func TestSpanReader_FindTracesSpanCollectionFailure(t *testing.T) {
	goodAggregations := make(map[string]json.RawMessage)
	rawMessage := []byte(`{"buckets": [{"key": "1","doc_count": 16},{"key": "2","doc_count": 16}]}`)
	goodAggregations[traceIDAggregation] = rawMessage

	badSpan := []byte(`{"TraceID": "123"asjlgajdfhilqghi[adfvca} bad json`)
	hits := make([]*elastic.SearchHit, 1)
	hits[0] = &elastic.SearchHit{
		Source: badSpan,
	}
	searchHits := &elastic.SearchHits{Hits: hits}

	withSpanReader(t, func(r *spanReaderTest) {
		mockSearchService(r).
			Return(&elastic.SearchResult{Aggregations: elastic.Aggregations(goodAggregations), Hits: searchHits}, nil)
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{
					{Hits: searchHits},
					{Hits: searchHits},
				},
			}, nil)

		traceQuery := dbmodel.TraceQueryParameters{
			ServiceName: serviceName,
			Tags: map[string]string{
				"hello": "world",
			},
			StartTimeMin: time.Now().Add(-1 * time.Hour),
			StartTimeMax: time.Now(),
		}

		traces, err := r.reader.FindTraces(context.Background(), traceQuery)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.Error(t, err)
		assert.Empty(t, traces)
	})
}

func TestFindTraceIDs(t *testing.T) {
	testCases := []struct {
		aggregrationID string
	}{
		{traceIDAggregation},
		{servicesAggregation},
		{operationsAggregation},
	}
	for _, testCase := range testCases {
		t.Run(testCase.aggregrationID, func(t *testing.T) {
			testGet(testCase.aggregrationID, t)
		})
	}
}

func TestReturnSearchFunc_DefaultCase(t *testing.T) {
	r := &spanReaderTest{}
	
	result, err := returnSearchFunc("unknownAggregationType", r)
	
	assert.Nil(t, result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Specify services, operations, traceIDs only")
}

func mockMultiSearchService(r *spanReaderTest) *mock.Call {
	multiSearchService := &mocks.MultiSearchService{}
	multiSearchService.On("Add", mock.Anything, mock.Anything, mock.Anything).Return(multiSearchService)
	multiSearchService.On("Index", mock.AnythingOfType("[]string")).Return(multiSearchService)
	r.client.On("MultiSearch").Return(multiSearchService)
	return multiSearchService.On("Do", mock.Anything)
}

func mockArchiveMultiSearchService(r *spanReaderTest, indexName []string) *mock.Call {
	multiSearchService := &mocks.MultiSearchService{}
	multiSearchService.On("Add", mock.Anything, mock.Anything, mock.Anything).Return(multiSearchService)
	multiSearchService.On("Index", indexName).Return(multiSearchService)
	r.client.On("MultiSearch").Return(multiSearchService)
	return multiSearchService.On("Do", mock.Anything)
}

// matchTermsAggregation uses reflection to match the size attribute of the TermsAggregation; neither
// attributes nor getters are exported by TermsAggregation.
func matchTermsAggregation(termsAgg *elastic.TermsAggregation) bool {
	val := reflect.ValueOf(termsAgg).Elem()
	sizeVal := val.FieldByName("size").Elem().Int()
	return sizeVal == defaultMaxDocCount
}

func mockSearchService(r *spanReaderTest) *mock.Call {
	searchService := &mocks.SearchService{}
	searchService.On("Query", mock.Anything).Return(searchService)
	searchService.On("IgnoreUnavailable", mock.AnythingOfType("bool")).Return(searchService)
	searchService.On("Size", mock.MatchedBy(func(size int) bool {
		return size == 0 // Aggregations apply size (bucket) limits in their own query objects, and do not apply at the parent query level.
	})).Return(searchService)
	searchService.On("Aggregation", stringMatcher(servicesAggregation), mock.MatchedBy(matchTermsAggregation)).Return(searchService)
	searchService.On("Aggregation", stringMatcher(operationsAggregation), mock.MatchedBy(matchTermsAggregation)).Return(searchService)
	searchService.On("Aggregation", stringMatcher(traceIDAggregation), mock.AnythingOfType("*elastic.TermsAggregation")).Return(searchService)
	r.client.On("Search", mock.AnythingOfType("[]string")).Return(searchService)
	return searchService.On("Do", mock.Anything)
}

func TestTraceQueryParameterValidation(t *testing.T) {
	tqp := dbmodel.TraceQueryParameters{
		ServiceName: "",
		Tags: map[string]string{
			"hello": "world",
		},
	}
	err := validateQuery(tqp)
	require.EqualError(t, err, ErrServiceNameNotSet.Error())

	tqp.ServiceName = serviceName

	tqp.StartTimeMin = time.Time{} // time.Unix(0,0) doesn't work because timezones
	tqp.StartTimeMax = time.Time{}
	err = validateQuery(tqp)
	require.EqualError(t, err, ErrStartAndEndTimeNotSet.Error())

	tqp.StartTimeMin = time.Now()
	tqp.StartTimeMax = time.Now().Add(-1 * time.Hour)
	err = validateQuery(tqp)
	require.EqualError(t, err, ErrStartTimeMinGreaterThanMax.Error())

	tqp.StartTimeMin = time.Now().Add(-1 * time.Hour)
	tqp.StartTimeMax = time.Now()
	err = validateQuery(tqp)
	require.NoError(t, err)

	tqp.DurationMin = time.Hour
	tqp.DurationMax = time.Minute
	err = validateQuery(tqp)
	require.EqualError(t, err, ErrDurationMinGreaterThanMax.Error())
}

func TestSpanReader_buildTraceIDAggregation(t *testing.T) {
	expectedStr := `{ "terms":{
            "field":"traceID",
            "size":123,
            "order":{
               "startTime":"desc"
            }
         },
         "aggregations": {
            "startTime" : { "max": {"field": "startTime"}}
         }}`
	withSpanReader(t, func(r *spanReaderTest) {
		traceIDAggregation := r.reader.buildTraceIDAggregation(123)
		actual, err := traceIDAggregation.Source()
		require.NoError(t, err)

		expected := make(map[string]any)
		json.Unmarshal([]byte(expectedStr), &expected)
		expected["terms"].(map[string]any)["size"] = 123
		expected["terms"].(map[string]any)["order"] = []any{map[string]string{"startTime": "desc"}}
		assert.EqualValues(t, expected, actual)
	})
}

func TestSpanReader_buildFindTraceIDsQuery(t *testing.T) {
	withSpanReader(t, func(r *spanReaderTest) {
		traceQuery := dbmodel.TraceQueryParameters{
			DurationMin:   time.Second,
			DurationMax:   time.Second * 2,
			StartTimeMin:  time.Time{},
			StartTimeMax:  time.Time{}.Add(time.Second),
			ServiceName:   "s",
			OperationName: "o",
			Tags: map[string]string{
				"hello": "world",
			},
		}

		actualQuery := r.reader.buildFindTraceIDsQuery(traceQuery)
		actual, err := actualQuery.Source()
		require.NoError(t, err)
		expectedQuery := elastic.NewBoolQuery().
			Must(
				r.reader.buildDurationQuery(time.Second, time.Second*2),
				r.reader.buildStartTimeQuery(time.Time{}, time.Time{}.Add(time.Second)),
				r.reader.buildServiceNameQuery("s"),
				r.reader.buildOperationNameQuery("o"),
				r.reader.buildTagQuery("hello", "world"),
			)
		expected, err := expectedQuery.Source()
		require.NoError(t, err)
		assert.Equal(t, expected, actual)
	})
}

func TestSpanReader_buildDurationQuery(t *testing.T) {
	expectedStr := `{ "range":
			{ "duration": {
				        "gte": 1000000,
				        "lte": 2000000 }
			}
		}`
	withSpanReader(t, func(r *spanReaderTest) {
		durationMin := time.Second
		durationMax := time.Second * 2
		durationQuery := r.reader.buildDurationQuery(durationMin, durationMax)
		actual, err := durationQuery.Source()
		require.NoError(t, err)

		expected := make(map[string]any)
		json.Unmarshal([]byte(expectedStr), &expected)
		// We need to do this because we cannot process a json into uint64.
		expected["range"].(map[string]any)["duration"].(map[string]any)["gte"] = model.DurationAsMicroseconds(durationMin)
		expected["range"].(map[string]any)["duration"].(map[string]any)["lte"] = model.DurationAsMicroseconds(durationMax)

		assert.EqualValues(t, expected, actual)
	})
}

func TestSpanReader_buildStartTimeQuery(t *testing.T) {
	expectedStr := `{ "range":
			{ "startTimeMillis": {
				         "gte": 1000000,
				         "lte": 2000000 }
			}
		}`
	withSpanReader(t, func(r *spanReaderTest) {
		startTimeMin := time.Time{}.Add(time.Second)
		startTimeMax := time.Time{}.Add(2 * time.Second)
		durationQuery := r.reader.buildStartTimeQuery(startTimeMin, startTimeMax)
		actual, err := durationQuery.Source()
		require.NoError(t, err)

		expected := make(map[string]any)
		json.Unmarshal([]byte(expectedStr), &expected)
		// We need to do this because we cannot process a json into uint64.
		expected["range"].(map[string]any)["startTimeMillis"].(map[string]any)["gte"] = model.TimeAsEpochMicroseconds(startTimeMin) / 1000
		expected["range"].(map[string]any)["startTimeMillis"].(map[string]any)["lte"] = model.TimeAsEpochMicroseconds(startTimeMax) / 1000

		assert.EqualValues(t, expected, actual)
	})
}

func TestSpanReader_buildServiceNameQuery(t *testing.T) {
	expectedStr := `{ "match": { "process.serviceName": { "query": "bat" }}}`
	withSpanReader(t, func(r *spanReaderTest) {
		serviceNameQuery := r.reader.buildServiceNameQuery("bat")
		actual, err := serviceNameQuery.Source()
		require.NoError(t, err)

		expected := make(map[string]any)
		json.Unmarshal([]byte(expectedStr), &expected)

		assert.EqualValues(t, expected, actual)
	})
}

func TestSpanReader_buildOperationNameQuery(t *testing.T) {
	expectedStr := `{ "match": { "operationName": { "query": "spook" }}}`
	withSpanReader(t, func(r *spanReaderTest) {
		operationNameQuery := r.reader.buildOperationNameQuery("spook")
		actual, err := operationNameQuery.Source()
		require.NoError(t, err)

		expected := make(map[string]any)
		json.Unmarshal([]byte(expectedStr), &expected)

		assert.EqualValues(t, expected, actual)
	})
}

func TestSpanReader_buildTagQuery(t *testing.T) {
	inStr, err := os.ReadFile("fixtures/query_01.json")
	require.NoError(t, err)
	withSpanReader(t, func(r *spanReaderTest) {
		tagQuery := r.reader.buildTagQuery("bat.foo", "spook")
		actual, err := tagQuery.Source()
		require.NoError(t, err)

		expected := make(map[string]any)
		json.Unmarshal(inStr, &expected)

		assert.EqualValues(t, expected, actual)
	})
}

func TestSpanReader_buildTagRegexQuery(t *testing.T) {
	inStr, err := os.ReadFile("fixtures/query_02.json")
	require.NoError(t, err)
	withSpanReader(t, func(r *spanReaderTest) {
		tagQuery := r.reader.buildTagQuery("bat.foo", "spo.*")
		actual, err := tagQuery.Source()
		require.NoError(t, err)

		expected := make(map[string]any)
		json.Unmarshal(inStr, &expected)

		assert.EqualValues(t, expected, actual)
	})
}

func TestSpanReader_buildTagRegexEscapedQuery(t *testing.T) {
	inStr, err := os.ReadFile("fixtures/query_03.json")
	require.NoError(t, err)
	withSpanReader(t, func(r *spanReaderTest) {
		tagQuery := r.reader.buildTagQuery("bat.foo", "spo\\*")
		actual, err := tagQuery.Source()
		require.NoError(t, err)

		expected := make(map[string]any)
		json.Unmarshal(inStr, &expected)

		assert.EqualValues(t, expected, actual)
	})
}

func TestSpanReader_GetEmptyIndex(t *testing.T) {
	withSpanReader(t, func(r *spanReaderTest) {
		mockSearchService(r).
			Return(&elastic.SearchResult{}, nil)
		mockMultiSearchService(r).
			Return(&elastic.MultiSearchResult{
				Responses: []*elastic.SearchResult{},
			}, nil)

		traceQuery := dbmodel.TraceQueryParameters{
			ServiceName: serviceName,
			Tags: map[string]string{
				"hello": "world",
			},
			StartTimeMin: time.Now().Add(-1 * time.Hour),
			StartTimeMax: time.Now(),
			NumTraces:    2,
		}

		services, err := r.reader.FindTraces(context.Background(), traceQuery)
		require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
		require.NoError(t, err)
		assert.Empty(t, services)
	})
}

func TestSpanReader_ArchiveTraces(t *testing.T) {
	testCases := []struct {
		useAliases bool
		suffix     string
		expected   string
	}{
		{false, "", "jaeger-span-"},
		{true, "", "jaeger-span-read"},
		{false, "foobar", "jaeger-span-"},
		{true, "foobar", "jaeger-span-foobar"},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("useAliases=%v suffix=%s", tc.useAliases, tc.suffix), func(t *testing.T) {
			withArchiveSpanReader(t, tc.useAliases, tc.suffix, func(r *spanReaderTest) {
				mockSearchService(r).
					Return(&elastic.SearchResult{}, nil)
				mockArchiveMultiSearchService(r, []string{tc.expected}).
					Return(&elastic.MultiSearchResult{
						Responses: []*elastic.SearchResult{},
					}, nil)
				query := []dbmodel.TraceID{}
				trace, err := r.reader.GetTraces(context.Background(), query)
				require.NoError(t, err)
				require.NotEmpty(t, r.traceBuffer.GetSpans(), "Spans recorded")
				require.Empty(t, trace)
			})
		})
	}
}

func TestBuildTraceByIDQuery(t *testing.T) {
	tests := []struct {
		traceID string
		query   elastic.Query
	}{
		{
			traceID: "0000000000000001",
			query:   elastic.NewTermQuery(traceIDField, "0000000000000001"),
		},
		{
			traceID: "00000000000000010000000000000001",
			query:   elastic.NewTermQuery(traceIDField, "00000000000000010000000000000001"),
		},
		{
			traceID: "ffffffffffffffffffffffffffffffff",
			query:   elastic.NewTermQuery(traceIDField, "ffffffffffffffffffffffffffffffff"),
		},
		{
			traceID: "0short-traceid",
			query:   elastic.NewTermQuery(traceIDField, "0short-traceid"),
		},
	}
	for _, test := range tests {
		t.Run(test.traceID, func(t *testing.T) {
			q := buildTraceByIDQuery(dbmodel.TraceID(test.traceID))
			assert.Equal(t, test.query, q)
		})
	}
}

func TestTerminateAfterNotSet(t *testing.T) {
	srcFn := getSourceFn(99)
	searchSource := srcFn(elastic.NewMatchAllQuery(), 1)
	sp, err := searchSource.Source()
	require.NoError(t, err)

	searchParams, ok := sp.(map[string]any)
	require.True(t, ok)

	termAfter, ok := searchParams["terminate_after"]
	require.False(t, ok)
	assert.Nil(t, termAfter)

	query, ok := searchParams["query"]
	require.True(t, ok)

	queryMap, ok := query.(map[string]any)
	require.True(t, ok)
	_, ok = queryMap["match_all"]
	require.True(t, ok)

	size, ok := searchParams["size"]
	require.True(t, ok)
	assert.Equal(t, 99, size)
}

func TestTagsMap(t *testing.T) {
	tests := []struct {
		fieldTags map[string]any
		expected  dbmodel.KeyValue
	}{
		{fieldTags: map[string]any{"bool:bool": true}, expected: dbmodel.KeyValue{Key: "bool.bool", Value: true, Type: dbmodel.BoolType}},
		{fieldTags: map[string]any{"int.int": int64(1)}, expected: dbmodel.KeyValue{Key: "int.int", Value: int64(1), Type: dbmodel.Int64Type}},
		{fieldTags: map[string]any{"int:int": int64(2)}, expected: dbmodel.KeyValue{Key: "int.int", Value: int64(2), Type: dbmodel.Int64Type}},
		{fieldTags: map[string]any{"float": float64(1.1)}, expected: dbmodel.KeyValue{Key: "float", Value: float64(1.1), Type: dbmodel.Float64Type}},
		{fieldTags: map[string]any{"float": float64(123)}, expected: dbmodel.KeyValue{Key: "float", Value: float64(123), Type: dbmodel.Float64Type}},
		{fieldTags: map[string]any{"float": float64(123.0)}, expected: dbmodel.KeyValue{Key: "float", Value: float64(123.0), Type: dbmodel.Float64Type}},
		{fieldTags: map[string]any{"float:float": float64(123)}, expected: dbmodel.KeyValue{Key: "float.float", Value: float64(123), Type: dbmodel.Float64Type}},
		{fieldTags: map[string]any{"json_number:int": json.Number("123")}, expected: dbmodel.KeyValue{Key: "json_number.int", Value: int64(123), Type: dbmodel.Int64Type}},
		{fieldTags: map[string]any{"json_number:float": json.Number("123.0")}, expected: dbmodel.KeyValue{Key: "json_number.float", Value: float64(123.0), Type: dbmodel.Float64Type}},
		{fieldTags: map[string]any{"json_number:err": json.Number("foo")}, expected: dbmodel.KeyValue{Key: "json_number.err", Value: "invalid tag type in foo: strconv.ParseFloat: parsing \"foo\": invalid syntax", Type: dbmodel.StringType}},
		{fieldTags: map[string]any{"str": "foo"}, expected: dbmodel.KeyValue{Key: "str", Value: "foo", Type: dbmodel.StringType}},
		{fieldTags: map[string]any{"str:str": "foo"}, expected: dbmodel.KeyValue{Key: "str.str", Value: "foo", Type: dbmodel.StringType}},
		{fieldTags: map[string]any{"binary": []byte("foo")}, expected: dbmodel.KeyValue{Key: "binary", Value: []byte("foo"), Type: dbmodel.BinaryType}},
		{fieldTags: map[string]any{"binary:binary": []byte("foo")}, expected: dbmodel.KeyValue{Key: "binary.binary", Value: []byte("foo"), Type: dbmodel.BinaryType}},
		{fieldTags: map[string]any{"unsupported": struct{}{}}, expected: dbmodel.KeyValue{Key: "unsupported", Value: fmt.Sprintf("invalid tag type in %+v", struct{}{}), Type: dbmodel.StringType}},
	}
	reader := NewSpanReader(SpanReaderParams{
		TagDotReplacement: ":",
		Logger:            zap.NewNop(),
	})
	for i, test := range tests {
		t.Run(fmt.Sprintf("%d, %s", i, test.fieldTags), func(t *testing.T) {
			tags := []dbmodel.KeyValue{
				{
					Key:   "testing-key",
					Type:  dbmodel.StringType,
					Value: "testing-value",
				},
			}
			spanTags := make(map[string]any)
			for k, v := range test.fieldTags {
				spanTags[k] = v
			}
			span := &dbmodel.Span{
				Process: dbmodel.Process{
					Tag:  test.fieldTags,
					Tags: tags,
				},
				Tag:  spanTags,
				Tags: tags,
			}
			reader.mergeAllNestedAndElevatedTagsOfSpan(span)
			tags = append(tags, test.expected)
			assert.Empty(t, span.Tag)
			assert.Empty(t, span.Process.Tag)
			assert.Equal(t, tags, span.Tags)
			assert.Equal(t, tags, span.Process.Tags)
		})
	}
}
