package main

//go:generate go-bindata -prefix ="assets/" -o assets.go assets/...

import (
	"encoding/json"
	"io/ioutil"
	"reflect"
	"regexp"
	"strings"

	"github.com/caarlos0/env"

	"github.com/iryonetwork/wwm/config"
	"github.com/iryonetwork/wwm/reports/generator"
)

// Config represents configuration of batchStorageSync
type Config struct {
	config.Config

	// filepaths to json spec files
	ReportSpecs ReportSpecs `env:"REPORT_SPECS_FILEPATHS" envDefault:"assets/encountersReportSpec.json,assets/patientsReportSpec.json"`

	DbUsername    string `env:"DB_USERNAME,required"`
	DbPassword    string `env:"DB_PASSWORD,required"`
	PGHost        string `env:"POSTGRES_HOST" envDefault:"postgres"`
	PGDatabase    string `env:"POSTGRES_DATABASE" envDefault:"reports"`
	PGRole        string `env:"POSTGRES_ROLE" envDefault:"reportgenerationservice"`
	DbDetailedLog bool   `env:"DB_DETAILED_LOG" envDefault:"false"`

	BoltDBFilepath string `env:"BOLT_DB_FILEPATH" envDefault:"/data/dataExportAndReportGeneration.db"`

	PrometheusPushGatewayAddress string `env:"PROMETHEUS_PUSH_GATEWAY_ADDRESS" envDefault:"http://localPrometheusPushGateway:9091"`
}

const assetsRe = "^assets/.+$"

// ReportSpecs is a wrapper struct for slice with ReportSpecs
// to make env parser to execute custom parser without "type not supported" error
type ReportSpecs struct {
	Slice []generator.ReportSpec
}

// getConfig parses environment variables and returns pointer to config and error
func getConfig() (*Config, error) {
	common, err := config.New()
	if err != nil {
		return nil, err
	}

	cfg := &Config{Config: *common}

	parsers := map[reflect.Type]env.ParserFunc{
		reflect.TypeOf(cfg.ReportSpecs): parseReportSpecs,
	}

	return cfg, env.ParseWithFuncs(cfg, parsers)
}

func parseReportSpecs(filepaths string) (interface{}, error) {
	filepathsSlice := strings.Split(filepaths, ",")

	reportSpecs := ReportSpecs{
		Slice: []generator.ReportSpec{},
	}

	re := regexp.MustCompile(assetsRe)

	for _, filepath := range filepathsSlice {
		spec := generator.ReportSpec{}
		var jsonFile []byte
		var err error

		match := re.FindString(filepath)
		if len(match) != 0 {
			jsonFile, err = Asset(match)
		} else {
			jsonFile, err = ioutil.ReadFile(filepath)
		}

		if err != nil {
			return nil, err
		}

		err = json.Unmarshal(jsonFile, &spec)
		if err != nil {
			return nil, err
		}
		reportSpecs.Slice = append(reportSpecs.Slice, spec)

	}

	return reportSpecs, nil
}
