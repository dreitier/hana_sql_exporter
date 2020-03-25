package cmd

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	goHdbDriver "github.com/SAP/go-hdb/driver"

	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/ulranh/hana_sql_exporter/internal"
)

type collector struct {
	// possible metric descriptions.
	Desc *prometheus.Desc

	// a parameterized function used to gather metrics.
	stats func() []metricData
}

type metricData struct {
	name       string
	help       string
	metricType string
	stats      []statData
}

type statData struct {
	value       float64
	labels      []string
	labelValues []string
}

// start collector and web server
func (config *Config) web(flags map[string]*string) error {

	var err error

	config.timeout, err = strconv.ParseUint(*flags["timeout"], 10, 0)
	if err != nil {
		exit(fmt.Sprint(" timeout flag has wrong type", err))
	}

	config.Tenants, err = config.prepareTenants()
	if err != nil {
		return errors.Wrap(err, " preparation of tenants not possible")
	}
	for _, t := range config.Tenants {
		defer t.conn.Close()
	}

	// add sys schema to SchemaFilter if it does not exists
	for _, m := range config.Metrics {
		if !containsString("sys", m.SchemaFilter) {
			m.SchemaFilter = append(m.SchemaFilter, "sys")
		}
	}

	stats := func() []metricData {
		data := config.collectMetrics()
		return data
	}

	// start collector
	c := newCollector(stats)
	prometheus.MustRegister(c)

	// start http server
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	server := &http.Server{
		Addr:         ":" + *flags["port"],
		Handler:      mux,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
	}
	err = server.ListenAndServe()
	if err != nil {
		return errors.Wrap(err, " web - ListenAndServe")
	}
	return nil
}

// start collecting all metrics and fetch the results
func (config *Config) collectMetrics() []metricData {

	var wg sync.WaitGroup

	metricsC := make(chan metricData, len(config.Metrics))
	for _, metric := range config.Metrics {

		wg.Add(1)
		go func(metric *metricInfo, tenants tenantsInfo) {

			defer wg.Done()
			metricsC <- metricData{
				name:       metric.Name,
				help:       metric.Help,
				metricType: metric.MetricType,
				stats:      tenants.collectMetric(metric, config.timeout),
			}
		}(metric, config.Tenants)
	}

	go func() {
		wg.Wait()
		close(metricsC)
	}()

	var metricsData []metricData
	for metric := range metricsC {
		metricsData = append(metricsData, metric)
	}

	return metricsData
}

// start collecting metric information for all tenants
func (tenants tenantsInfo) collectMetric(metric *metricInfo, timeout uint64) []statData {

	metricC := make(chan []statData, len(tenants))

	for _, tenant := range tenants {

		go func(metric *metricInfo, tenant *tenantInfo) {

			metricC <- tenant.prepareMetricData(metric)
		}(metric, tenant)
	}

	i := 0
	var sData []statData
	timeAfter := time.After(time.Duration(timeout) * time.Second)

stopReading:
	for {
		select {
		case mc := <-metricC:
			if mc != nil {
				sData = append(sData, mc...)
			}
			i += 1
			if len(tenants) == i {
				break stopReading
			}
		case <-timeAfter:
			break stopReading
		}
	}
	return sData
}

// filter out not associated tenants
func (tenant *tenantInfo) prepareMetricData(metric *metricInfo) []statData {

	// all values of metrics tag filter must be in tenants tags, otherwise the
	// metric is not relevant for the tenant
	if !subSliceInSlice(metric.TagFilter, tenant.Tags) {
		return nil
	}

	sel := strings.TrimSpace(metric.SQL)
	if !strings.EqualFold(sel[0:6], "select") {
		log.WithFields(log.Fields{
			"metric": metric.Name,
			"tenant": tenant.Name,
		}).Error("Only selects are allowed")
		return nil
	}

	// metrics schema filter must include a tenant schema
	var schema string
	if schema = firstValueInSlice(metric.SchemaFilter, tenant.schemas); 0 == len(schema) {
		log.WithFields(log.Fields{
			"metric": metric.Name,
			"tenant": tenant.Name,
		}).Error("SchemaFilter value in toml file is missing")
		return nil
	}
	sel = strings.ReplaceAll(sel, "<SCHEMA>", schema)

	res, err := tenant.getMetricData(sel)
	if err != nil {
		log.WithFields(log.Fields{
			"metric": metric.Name,
			"tenant": tenant.Name,
			"error":  err,
		}).Error("Can't get sql result for metric")
		return nil
	}
	return res
}

// get metric data for one tenant
func (tenant *tenantInfo) getMetricData(sel string) ([]statData, error) {
	var err error

	var rows *sql.Rows
	rows, err = tenant.conn.Query(sel)
	if err != nil {
		return nil, errors.Wrap(err, "GetSqlData - query")
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, errors.Wrap(err, "GetSqlData - columns")
	}

	if len(cols) < 1 {
		return nil, errors.Wrap(err, "GetSqlData - no columns")
	}

	// first column must not be string
	colt, err := rows.ColumnTypes()
	if err != nil {
		return nil, errors.Wrap(err, "GetSqlData - columnTypes")
	}
	switch colt[0].ScanType().Name() {
	case "string", "bool", "":
		return nil, errors.New("GetSqlData - first column must be numeric")
	default:
	}

	values := make([]sql.RawBytes, len(cols))
	scanArgs := make([]interface{}, len(values))
	for i := range values {
		scanArgs[i] = &values[i]
	}

	var md []statData
	for rows.Next() {
		data := statData{
			labels:      []string{"tenant", "usage"},
			labelValues: []string{strings.ToLower(tenant.Name), strings.ToLower(tenant.usage)},
		}
		err = rows.Scan(scanArgs...)
		if err != nil {
			return nil, errors.Wrap(err, "GetSqlData - rows.Scan")
		}

		for i, colval := range values {

			// check for NULL value
			if colval == nil {
				return nil, errors.Wrap(err, "GetSqlData - colval is null")
			}

			if 0 == i {

				// the first column must be the float value
				data.value, err = strconv.ParseFloat(string(colval), 64)
				if err != nil {
					return nil, errors.Wrap(err, "GetSqlData - first column cannot be converted to float64")
				}
			} else {
				data.labels = append(data.labels, strings.ToLower(cols[i]))
				data.labelValues = append(data.labelValues, strings.ToLower(strings.Join(strings.Split(string(colval), " "), "_")))

			}
		}
		md = append(md, data)
	}
	if err = rows.Err(); err != nil {
		return nil, errors.Wrap(err, "GetSqlData - rows")
	}
	return md, nil
}

func newCollector(stats func() []metricData) *collector {
	return &collector{
		stats: stats,
	}
}

// describe implements prometheus.Collector.
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

// collect implements prometheus.Collector.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	// take a stats snapshot. must be concurrency safe.
	stats := c.stats()

	var valueType = map[string]prometheus.ValueType{
		"gauge":   prometheus.GaugeValue,
		"counter": prometheus.CounterValue,
	}

	for _, mi := range stats {
		for _, v := range mi.stats {
			m := prometheus.MustNewConstMetric(
				prometheus.NewDesc(mi.name, mi.help, v.labels, nil),
				valueType[strings.ToLower(mi.metricType)],
				v.value,
				v.labelValues...,
			)
			ch <- m
		}
	}
}

// helper functions

// add missing information to tenant struct
func (config *Config) prepareTenants() (tenantsInfo, error) {

	var tenantsOk tenantsInfo

	// unmarshal secret byte array
	var secret internal.Secret
	if err := proto.Unmarshal(config.Secret, &secret); err != nil {
		return nil, errors.Wrap(err, " unable to unmarshal secret")
	}

	for i := 0; i < len(config.Tenants); i++ {

		pw, err := getPW(secret, config.Tenants[i].Name)
		if err != nil {
			log.WithFields(log.Fields{
				"tenant": config.Tenants[i].Name,
				"error":  err,
			}).Error("Can't find or decrypt password for tenant - tenant removed!")

			continue
		}

		// connect to db tenant
		config.Tenants[i].conn = dbConnect(config.Tenants[i].ConnStr, config.Tenants[i].User, pw)
		err = config.Tenants[i].conn.Ping()
		if err != nil {
			log.WithFields(log.Fields{
				"tenant": config.Tenants[i].Name,
				"error":  err,
			}).Error("Can't connect to tenant - tenant removed!")
			continue
		}

		// get tenant usage and hana-user schema information
		err = config.Tenants[i].collectRemainingTenantInfos()
		if err != nil {
			log.WithFields(log.Fields{
				"tenant": config.Tenants[i].Name,
				"error":  err,
			}).Error("Problems with select of remaining tenant info - tenant removed!")

			continue
		}
		tenantsOk = append(tenantsOk, config.Tenants[i])
	}
	return tenantsOk, nil

}

// decrypt password
func getPW(secret internal.Secret, name string) (string, error) {

	// get encrypted tenant pw
	if _, ok := secret.Name[name]; !ok {
		return "", errors.New("encrypted tenant pw info does not exist")
	}

	// decrypt tenant password
	pw, err := internal.PwDecrypt(secret.Name[name], secret.Name["secretkey"])
	if err != nil {
		return "", err
	}
	return pw, nil
}

// connect to database
func dbConnect(connStr, user, pw string) *sql.DB {

	connector, err := goHdbDriver.NewDSNConnector("hdb://" + user + ":" + pw + "@" + connStr)
	if err != nil {
		log.Fatal(err)
	}
	// connector.SetTimeout(timeout)
	return sql.OpenDB(connector)
}

// get tenant usage and hana-user schema information
func (t *tenantInfo) collectRemainingTenantInfos() error {

	// get tenant usage information
	row := t.conn.QueryRow("select usage from sys.m_database")
	err := row.Scan(&t.usage)
	if err != nil {
		return err
	}

	// append sys schema to tenant schemas
	t.schemas = append(t.schemas, "sys")

	// append remaining user schema privileges
	rows, err := t.conn.Query("select schema_name from sys.granted_privileges where object_type='SCHEMA' and grantee=$1", strings.ToUpper(t.User))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var schema string
		err := rows.Scan(&schema)
		if err != nil {
			return err
		}
		t.schemas = append(t.schemas, schema)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return nil
}

// true, if slice contains string
func containsString(str string, slice []string) bool {
	for _, s := range slice {
		if strings.EqualFold(s, str) {
			return true
		}
	}
	return false
}

// true, if every item in sublice exists in slice or sublice is empty
func subSliceInSlice(subSlice []string, slice []string) bool {
	for _, vs := range subSlice {
		for _, v := range slice {
			if strings.EqualFold(vs, v) {
				goto nextCheck
			}
		}
		return false
	nextCheck:
	}
	return true
}

// return first sublice value that exists in slice
func firstValueInSlice(subSlice []string, slice []string) string {
	for _, vs := range subSlice {
		for _, v := range slice {
			if strings.EqualFold(vs, v) {
				return vs
			}
		}
	}
	return ""
}
