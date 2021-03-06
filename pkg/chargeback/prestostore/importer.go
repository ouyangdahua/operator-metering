package prestostore

import (
	"context"
	"fmt"
	"sync"
	"time"

	prom "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/clock"

	"github.com/operator-framework/operator-metering/pkg/presto"
	"github.com/operator-framework/operator-metering/pkg/promquery"
)

const (
	// cap the maximum c.cfg.ChunkSize
	maxChunkDuration = 24 * time.Hour
)

// PrometheusImporter imports Prometheus metrics into Presto tables
type PrometheusImporter struct {
	logger          logrus.FieldLogger
	promConn        prom.API
	prestoQueryer   presto.ExecQueryer
	collectHandlers promquery.ResultHandler
	clock           clock.Clock
	cfg             Config

	// importLock ensures only one import is running at a time, protecting the
	// lastTimestamp and metrics fields
	importLock sync.Mutex

	//lastTimestamp is the lastTimestamp stored for this PrometheusImporter
	lastTimestamp *time.Time
}

type Config struct {
	PrometheusQuery       string
	PrestoTableName       string
	ChunkSize             time.Duration
	StepSize              time.Duration
	MaxTimeRanges         int64
	AllowIncompleteChunks bool
}

func NewPrometheusImporter(logger logrus.FieldLogger, promConn prom.API, prestoQueryer presto.ExecQueryer, clock clock.Clock, cfg Config) *PrometheusImporter {
	logger = logger.WithFields(logrus.Fields{
		"component": "PrometheusImporter",
		"tableName": cfg.PrestoTableName,
	})

	var metricsCount int
	preProcessingHandler := func(_ context.Context, timeRanges []prom.Range) error {
		metricsCount = 0
		if len(timeRanges) == 0 {
			logger.Info("no time ranges to query yet for table %s", cfg.PrestoTableName)
		} else {
			begin := timeRanges[0].Start.UTC()
			end := timeRanges[len(timeRanges)-1].End.UTC()
			logger.Infof("querying for data between %s and %s (chunks: %d)", begin, end, len(timeRanges))
		}
		return nil
	}

	preQueryHandler := func(ctx context.Context, timeRange prom.Range) error {
		logger.Debugf("querying Prometheus using range %v to %v", timeRange.Start, timeRange.End)
		return nil
	}

	postQueryHandler := func(ctx context.Context, timeRange prom.Range, matrix model.Matrix) error {
		metrics := promMatrixToPrometheusMetrics(timeRange, matrix)
		logger.Debugf("got %d metrics, storing them into Presto into table %s", len(metrics), cfg.PrestoTableName)
		err := StorePrometheusMetrics(ctx, prestoQueryer, cfg.PrestoTableName, metrics)
		if err != nil {
			return fmt.Errorf("failed to store Prometheus metrics into table %s for the range %v to %v: %v",
				cfg.PrestoTableName, timeRange.Start, timeRange.End, err)
		}
		logger.Debugf("stored %d metrics into Presto table %s successfully", len(metrics), cfg.PrestoTableName)
		metricsCount += len(metrics)
		return nil
	}

	postProcessingHandler := func(_ context.Context, timeRanges []prom.Range) error {
		begin := timeRanges[0].Start.UTC()
		end := timeRanges[len(timeRanges)-1].End.UTC()
		logger.Debugf("stored a total of %d metrics for data between %s and %s into %s", metricsCount, begin, end, cfg.PrestoTableName)
		metricsCount = 0
		return nil
	}

	collectHandlers := promquery.ResultHandler{
		PreProcessingHandler:  preProcessingHandler,
		PreQueryHandler:       preQueryHandler,
		PostQueryHandler:      postQueryHandler,
		PostProcessingHandler: postProcessingHandler,
	}

	return &PrometheusImporter{
		logger:          logger,
		promConn:        promConn,
		prestoQueryer:   prestoQueryer,
		collectHandlers: collectHandlers,
		clock:           clock,
		cfg:             cfg,
	}
}

func (c *PrometheusImporter) UpdateConfig(cfg Config) {
	c.importLock.Lock()
	c.cfg = cfg
	c.importLock.Unlock()
}

// ImportFromLastTimestamp executes a Presto query from the last time range it
// queried and stores the results in a Presto table.

// The importer will track the last time series it retrieved and will query
// the next time range starting from where it left off if paused or stopped.
// For more details on how querying Prometheus is done, see the package
// pkg/promquery.
func (c *PrometheusImporter) ImportFromLastTimestamp(ctx context.Context) ([]prom.Range, error) {
	c.importLock.Lock()
	logger := c.logger
	logger.Infof("PrometheusImporter ImportFromLastTimestamp started")
	defer logger.Infof("PrometheusImporter ImportFromLastTimestamp finished")
	defer c.importLock.Unlock()

	endTime := c.clock.Now().UTC()

	// if c.lastTimestamp is null then it's because we errored sometime
	// last time we collected and need to re-query Presto to figure out
	// the last timestamp
	if c.lastTimestamp == nil {
		var err error
		c.lastTimestamp, err = getLastTimestampForTable(c.prestoQueryer, c.cfg.PrestoTableName)
		if err != nil {
			logger.WithError(err).Errorf("unable to get last timestamp for table %s", c.cfg.PrestoTableName)
			return nil, err
		}
	}

	var startTime time.Time
	if c.lastTimestamp != nil {
		c.logger.Debugf("got lastTimestamp for table %s: %s", c.cfg.PrestoTableName, c.lastTimestamp.String())

		// We don't want to duplicate the c.lastTimestamp metric so add
		// the step size so that we start at the next interval no longer in
		// our range.
		startTime = c.lastTimestamp.Add(c.cfg.StepSize)
	} else {
		// Looks like we haven't populated any data in this table yet.
		// Let's backfill our last 1 chunk.
		// we multiple by 2 because the most recent chunk will have a
		// chunkEnd == endTime, so it won't be queried, so this gets the chunk
		// before the latest
		startTime = endTime.Add(-2 * c.cfg.ChunkSize)
		c.logger.Debugf("no data in data store %s yet", c.cfg.PrestoTableName)
	}

	// If the startTime is too far back, we should limit this run to
	// maxChunkDuration so that if we're stopped for an extended amount of time,
	// this function won't return a slice with too many time ranges.
	totalChunkDuration := startTime.Sub(endTime)
	if totalChunkDuration >= maxChunkDuration {
		endTime = startTime.Add(maxChunkDuration)
	}

	loggerWithFields := logger.WithFields(logrus.Fields{
		"startTime": startTime,
		"endTime":   endTime,
	})

	return c.importMetrics(loggerWithFields, ctx, startTime, endTime)
}

func (c *PrometheusImporter) ImportMetrics(ctx context.Context, startTime, endTime time.Time) ([]prom.Range, error) {
	c.importLock.Lock()
	logger := c.logger.WithFields(logrus.Fields{
		"startTime": startTime,
		"endTime":   endTime,
	})
	logger.Infof("PrometheusImporter Import started")
	defer logger.Infof("PrometheusImporter Import finished")
	defer c.importLock.Unlock()

	return c.importMetrics(logger, ctx, startTime, endTime)
}

func (c *PrometheusImporter) importMetrics(logger logrus.FieldLogger, ctx context.Context, startTime, endTime time.Time) ([]prom.Range, error) {
	logger.Debugf("importing metrics between %s and %s", startTime, endTime)
	timeRanges, err := promquery.QueryRangeChunked(ctx, c.promConn, c.cfg.PrometheusQuery, startTime, endTime, c.cfg.ChunkSize, c.cfg.StepSize, c.cfg.MaxTimeRanges, c.cfg.AllowIncompleteChunks, c.collectHandlers)
	if err != nil {
		logger.WithError(err).Error("error collecting metrics")
		// at this point we cannot be sure what is in Presto and what
		// isn't, so reset our c.lastTimestamp
		c.lastTimestamp = nil
		return timeRanges, err
	}

	if len(timeRanges) == 0 {
		logger.Infof("no data collected for table %s", c.cfg.PrestoTableName)
	} else {
		lastTS := timeRanges[len(timeRanges)-1].End
		c.lastTimestamp = &lastTS
	}

	return timeRanges, nil
}

func promMatrixToPrometheusMetrics(timeRange prom.Range, matrix model.Matrix) []*PrometheusMetric {
	var metrics []*PrometheusMetric
	// iterate over segments of contiguous billing metrics
	for _, sampleStream := range matrix {
		for _, value := range sampleStream.Values {
			labels := make(map[string]string, len(sampleStream.Metric))
			for k, v := range sampleStream.Metric {
				labels[string(k)] = string(v)
			}

			metric := &PrometheusMetric{
				Labels:    labels,
				Amount:    float64(value.Value),
				StepSize:  timeRange.Step,
				Timestamp: value.Timestamp.Time().UTC(),
			}
			metrics = append(metrics, metric)
		}
	}
	return metrics
}
