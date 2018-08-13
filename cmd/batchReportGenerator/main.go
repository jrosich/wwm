package main

//go:generate sh -c "mkdir -p ../../gen/reportsStorage/ && swagger generate client -A reportsStorage -t ../../gen/reportsStorage/ -f ../../docs/api/reportsStorage.yml --principal string"

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io/ioutil"
	"os"
	"time"

	"github.com/go-openapi/runtime"
	runtimeClient "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	"github.com/iryonetwork/wwm/storage/keyvalue"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	"github.com/rs/zerolog"

	"github.com/iryonetwork/wwm/gen/reportsStorage/client"
	"github.com/iryonetwork/wwm/gen/reportsStorage/client/operations"
	"github.com/iryonetwork/wwm/reports/generator"
	"github.com/iryonetwork/wwm/service/serviceAuthenticator"
	reportsStorage "github.com/iryonetwork/wwm/storage/reports"
)

const (
	exporterBucket      string = "batchDataExporter"
	exporterStorageKey  string = "lastSuccessfulRun"
	generatorStorageKey string = "lastDataUntil"
)

func main() {
	// initialize logger
	logger := zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", "batchReportGenerator").
		Logger()

	// create context with cancel func
	ctx, cancelContext := context.WithCancel(context.Background())
	defer cancelContext()

	// get config
	cfg, err := getConfig()
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to get config")
	}

	// initialize promethues metrics registry
	metricsRegistry := prometheus.NewRegistry()

	// connect to database
	connStr := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=require",
		cfg.DbUsername,
		cfg.DbPassword,
		cfg.PGHost,
		cfg.PGDatabase)
	db, err := gorm.Open("postgres", connStr)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to initialize database connection")
	}
	db.LogMode(cfg.DbDetailedLog)

	// switch roles
	tx := db.Exec(fmt.Sprintf("SET ROLE '%s'", cfg.PGRole))
	if err := tx.Error; err != nil {
		logger.Fatal().Err(err).Msg("Failed to switch database roles")
	}

	// initialize storage
	storage, err := reportsStorage.New(ctx, db, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to initialize reports storage")
	}

	// initialize reports storage API client
	rc := runtimeClient.New(cfg.StorageHost, cfg.StoragePath, []string{"https"})
	filesStorageClient := client.New(rc, strfmt.Default)

	// initialize request authenticator
	auth, err := serviceAuthenticator.New(cfg.CertPath, cfg.KeyPath, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to initialize reports storage API request authenticator")
	}

	// initalize generator
	g, err := generator.New(storage, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to initialize generator")
	}

	// initialize bolt key value storage to read last succesful run
	s, err := keyvalue.NewBolt(ctx, cfg.BoltDBFilepath, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to initialize key value storage")
	}
	// get metrics collection for key value storage and register in registry
	m := s.GetPrometheusMetricsCollection()
	for _, metric := range m {
		metricsRegistry.MustRegister(metric)
	}

	// initialize prometheus metrics pusher
	metricsPusher := push.New(cfg.PrometheusPushGatewayAddress, "batchReportGenerator").Gatherer(metricsRegistry)

	zeroTime := strfmt.DateTime(time.Unix(0, 0))

	exporterLastSuccessfulRun := zeroTime
	v := s.Get(exporterBucket, exporterStorageKey)
	if v != nil {
		timestamp, err := strfmt.ParseDateTime(string(v))
		if err == nil {
			exporterLastSuccessfulRun = timestamp
		}
	}

	for _, spec := range cfg.ReportSpecs.Slice {
		// initialize csv writer
		file, err := ioutil.TempFile("", spec.Type)
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to open temporary report file")
		}
		defer os.Remove(file.Name())
		writer := csv.NewWriter(file)
		writer.Comma = ';'

		// initialize last successful run with 0 value
		generatorLastDataUntil := zeroTime
		if !spec.IncludeAll {
			// try to read last succesful run
			v := s.Get(spec.Type, generatorStorageKey)
			if v != nil {
				timestamp, err := strfmt.ParseDateTime(string(v))
				if err == nil {
					generatorLastDataUntil = timestamp
				}
			}
		}

		if time.Time(generatorLastDataUntil).After(time.Time(exporterLastSuccessfulRun)) ||
			time.Time(generatorLastDataUntil).Equal(time.Time(exporterLastSuccessfulRun)) {
			logger.Info().Msg("BatchDataExporter did not export anything since last `dataUntil` of last report of this type, skip generating report")
			break
		}

		// run generator
		generated, err := g.Generate(ctx, writer, spec, &generatorLastDataUntil, nil)
		if err != nil {
			logger.Fatal().Err(err).Msgf("failed to generate report file %s", spec.Type)
		}

		if generated {
			// flush writer
			writer.Flush()

			// read file and turn content into buffer
			b, err := ioutil.ReadFile(file.Name())
			if err != nil {
				logger.Fatal().Err(err).Msgf("failed to read temp file")
			}
			buf := bytes.NewBuffer(b)

			// upload file to storage
			reportNewParams := operations.NewReportNewParams().
				WithContentType("text/csv").
				WithDataSince(&generatorLastDataUntil).
				WithDataUntil(exporterLastSuccessfulRun).
				WithReportType(spec.Type).
				WithFile(runtime.NamedReader("reader", buf))

			ok, err := filesStorageClient.Operations.ReportNew(reportNewParams, auth)
			if err != nil {
				logger.Fatal().Err(err).Msgf("failed to upload report file %s", spec.Type)
			}

			logger.Info().Msgf("report %s was uploaded as file %s", ok.Payload.ReportType, ok.Payload.Name)

			// save lastSuccesfulRun of exporter as `dataUntil` of current generator run
			s.Update(spec.Type, generatorStorageKey, []byte(exporterLastSuccessfulRun.String()))
		}
	}

	// push metrics to the push gateway
	err = metricsPusher.Add()
	if err != nil {
		logger.Error().Err(err).Msg("failed to push metrics to push gateway")
	}
}
