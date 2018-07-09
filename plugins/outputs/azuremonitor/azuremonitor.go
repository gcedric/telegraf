package azuremonitor

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/metric"
	"github.com/influxdata/telegraf/plugins/outputs"
)

var _ telegraf.AggregatingOutput = (*AzureMonitor)(nil)
var _ telegraf.Output = (*AzureMonitor)(nil)

// AzureMonitor allows publishing of metrics to the Azure Monitor custom metrics service
type AzureMonitor struct {
	useMsi              bool              `toml:"use_managed_service_identity"`
	ResourceID          string            `toml:"resource_id"`
	Region              string            `toml:"region"`
	Timeout             internal.Duration `toml:"Timeout"`
	AzureSubscriptionID string            `toml:"azure_subscription"`
	AzureTenantID       string            `toml:"azure_tenant"`
	AzureClientID       string            `toml:"azure_client_id"`
	AzureClientSecret   string            `toml:"azure_client_secret"`
	StringAsDimension   bool              `toml:"string_as_dimension"`

	url         string
	msiToken    *msiToken
	oauthConfig *adal.OAuthConfig
	adalToken   adal.OAuthTokenProvider

	client *http.Client

	cache map[time.Time]map[uint64]*aggregate
}

type aggregate struct {
	telegraf.Metric
	updated bool
}

const (
	defaultMSIResource     string = "https://monitoring.azure.com/"
	urlTemplate            string = "https://%s.monitoring.azure.com%s/metrics"
	resourceIDTemplate     string = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s"
	vmInstanceMetadataURL  string = "http://169.254.169.254/metadata/instance?api-version=2017-12-01"
	msiInstanceMetadataURL string = "http://169.254.169.254/metadata/identity/oauth2/token"
)

var sampleConfig = `
  ## The resource ID against which metric will be logged.  If not
  ## specified, the plugin will attempt to retrieve the resource ID
  ## of the VM via the instance metadata service (optional if running 
  ## on an Azure VM with MSI)
  #resource_id = "/subscriptions/<subscription_id>/resourceGroups/<resource_group>/providers/Microsoft.Compute/virtualMachines/<vm_name>"
  ## Azure region to publish metrics against.
  ## Leave blank to automatically query the region via MSI.
  ## Region must be manually set or acquired by MSI.
  #region = ""

  ## Write HTTP timeout, formatted as a string.  If not provided, will default
  ## to 5s. 0s means no timeout (not recommended).
  # timeout = "5s"

  ## Whether or not to use managed service identity.
  #use_managed_service_identity = true

  ## *The following fields are required if MSI is not used.*
  ## Fill in the following values if using Active Directory Service
  ## Principal or User Principal for authentication.
  ## Subscription ID
  #azure_subscription = ""
  ## Tenant ID
  #azure_tenant = ""
  ## Client ID
  #azure_client_id = ""
  ## Client secrete
  #azure_client_secret = ""
`

// Description provides a description of the plugin
func (a *AzureMonitor) Description() string {
	return "Configuration for sending aggregate metrics to Azure Monitor"
}

// SampleConfig provides a sample configuration for the plugin
func (a *AzureMonitor) SampleConfig() string {
	return sampleConfig
}

// Connect initializes the plugin and validates connectivity
func (a *AzureMonitor) Connect() error {
	a.client = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
		Timeout: a.Timeout.Duration,
	}

	// If no direct AD values provided, fall back to MSI
	if a.AzureSubscriptionID == "" && a.AzureTenantID == "" && a.AzureClientID == "" && a.AzureClientSecret == "" {
		a.useMsi = true
	} else if a.AzureSubscriptionID == "" || a.AzureTenantID == "" || a.AzureClientID == "" || a.AzureClientSecret == "" {
		return fmt.Errorf("must provide values for azure_subscription, azure_tenant, azure_client and azure_client_secret, or leave all blank to default to MSI")
	}

	if !a.useMsi {
		// If using direct AD authentication create the AD access client
		oauthConfig, err := adal.NewOAuthConfig(azure.PublicCloud.ActiveDirectoryEndpoint, a.AzureTenantID)
		if err != nil {
			return fmt.Errorf("could not initialize AD client: %s", err)
		}
		a.oauthConfig = oauthConfig
	}

	// Pull region and resource identifier
	err := a.GetInstanceMetadata()
	if err != nil && a.ResourceID == "" {
		return fmt.Errorf("no resource ID specified or available via MSI")
	} else if a.Region == "" {
		return fmt.Errorf("no region not specified and not available via MSI")
	}

	err = a.validateCredentials()
	if err != nil {
		return fmt.Errorf("unable to fetch authentication credentials: %v", err)
	}

	a.Reset()

	return nil
}

func (a *AzureMonitor) validateCredentials() error {
	if a.useMsi {
		// Check expiry on the token
		if a.msiToken == nil || a.msiToken.expiresInDuration() < time.Minute {
			msiToken, err := a.getMsiToken(a.AzureClientID)
			if err != nil {
				return err
			}
			a.msiToken = msiToken
		}
		return nil
	}

	adalToken, err := adal.NewServicePrincipalToken(
		*(a.oauthConfig), a.AzureClientID, a.AzureClientSecret,
		azure.PublicCloud.ActiveDirectoryEndpoint)
	if err != nil {
		return fmt.Errorf("could not acquire ADAL token: %s", err)
	}
	a.adalToken = adalToken
	return nil
}

// Close shuts down an any active connections
func (a *AzureMonitor) Close() error {
	a.client = nil
	return nil
}

type azureMonitorMetric struct {
	Time time.Time         `json:"time"`
	Data *azureMonitorData `json:"data"`
}

type azureMonitorData struct {
	BaseData *azureMonitorBaseData `json:"baseData"`
}

type azureMonitorBaseData struct {
	Metric         string                `json:"metric"`
	Namespace      string                `json:"namespace"`
	DimensionNames []string              `json:"dimNames"`
	Series         []*azureMonitorSeries `json:"series"`
}

type azureMonitorSeries struct {
	DimensionValues []string `json:"dimValues"`
	Min             float64  `json:"min"`
	Max             float64  `json:"max"`
	Sum             float64  `json:"sum"`
	Count           int64    `json:"count"`
}

// Write writes metrics to the remote endpoint
func (a *AzureMonitor) Write(metrics []telegraf.Metric) error {
	azmetrics := make(map[uint64]*azureMonitorMetric, len(metrics))
	for _, m := range metrics {
		id := hashIDWithTagKeysOnly(m)
		if azm, ok := azmetrics[id]; !ok {
			azmetrics[id] = translate(m)
		} else {
			azmetrics[id].Data.BaseData.Series = append(
				azm.Data.BaseData.Series,
				translate(m).Data.BaseData.Series...,
			)
		}
	}

	var body []byte
	for _, m := range azmetrics {
		// Azure Monitor accepts new batches of points in new-line delimited
		// JSON, following RFC 4288 (see https://github.com/ndjson/ndjson-spec).
		jsonBytes, err := json.Marshal(&m)
		if err != nil {
			return err
		}
		body = append(body, jsonBytes...)
		body = append(body, '\n')
	}

	if err := a.validateCredentials(); err != nil {
		return fmt.Errorf("E! [outputs.azuremonitor] Unable to fetch authentication credentials: %v", err)
	}

	req, err := http.NewRequest("POST", a.url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	if a.adalToken != nil {
		req.Header.Set("Authorization", "Bearer "+a.adalToken.OAuthToken())
	} else {
		req.Header.Set("Authorization", "Bearer "+a.msiToken.AccessToken)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	rbody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		rbody = nil
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("E! Failed to write to [%s]: %v", a.ResourceID, rbody)
	}

	return nil
}

func hashIDWithTagKeysOnly(m telegraf.Metric) uint64 {
	h := fnv.New64a()
	h.Write([]byte(m.Name()))
	h.Write([]byte("\n"))
	for _, tag := range m.TagList() {
		h.Write([]byte(tag.Key))
		h.Write([]byte("\n"))
	}
	b := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(b, uint64(m.Time().UnixNano()))
	h.Write(b[:n])
	h.Write([]byte("\n"))
	return h.Sum64()
}

func translate(m telegraf.Metric) *azureMonitorMetric {
	var dimensionNames []string
	var dimensionValues []string
	for i, tag := range m.TagList() {
		// Azure custom metrics service supports up to 10 dimensions
		if i > 10 {
			log.Printf("W! [outputs.azuremonitor] metric [%s] exceeds 10 dimensions", m.Name())
			continue
		}
		dimensionNames = append(dimensionNames, tag.Key)
		dimensionValues = append(dimensionValues, tag.Value)
	}

	min, _ := m.GetField("min")
	max, _ := m.GetField("max")
	sum, _ := m.GetField("sum")
	count, _ := m.GetField("count")
	return &azureMonitorMetric{
		Time: m.Time(),
		Data: &azureMonitorData{
			BaseData: &azureMonitorBaseData{
				Metric:         m.Name(),
				Namespace:      "Telegraf/" + strings.SplitN(m.Name(), "-", 1)[0],
				DimensionNames: dimensionNames,
				Series: []*azureMonitorSeries{
					&azureMonitorSeries{
						DimensionValues: dimensionValues,
						Min:             min.(float64),
						Max:             max.(float64),
						Sum:             sum.(float64),
						Count:           count.(int64),
					},
				},
			},
		},
	}
}

// Add will append a metric to the output aggregate
func (a *AzureMonitor) Add(m telegraf.Metric) {
	// Azure Monitor only supports aggregates 30 minutes into the past
	// and 4 minutes into the future. Future metrics are dropped when pushed.
	t := m.Time()
	tbucket := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, t.Location())
	if tbucket.Before(time.Now().Add(-time.Minute * 30)) {
		log.Printf("W! attempted to aggregate metric over 30 minutes old: %v, %v", t, tbucket)
		return
	}

	// Azure Monitor doesn't have a string value type, so convert string
	// fields to dimensions (a.k.a. tags) if enabled.
	if a.StringAsDimension {
		for fk, fv := range m.Fields() {
			if v, ok := fv.(string); ok {
				m.AddTag(fk, v)
			}
		}
	}

	for _, f := range m.FieldList() {
		fv, ok := convert(f.Value)
		if !ok {
			continue
		}

		// Azure Monitor does not support fields so the field
		// name is appended to the metric name.
		name := m.Name() + "-" + sanitize(f.Key)
		id := hashIDWithField(m.HashID(), f.Key)

		_, ok = a.cache[tbucket]
		if !ok {
			// Time bucket does not exist and needs to be created.
			a.cache[tbucket] = make(map[uint64]*aggregate)
		}

		nf := make(map[string]interface{}, 4)
		nf["min"] = fv
		nf["max"] = fv
		nf["sum"] = fv
		nf["count"] = 1
		// Fetch existing aggregate
		agg, ok := a.cache[tbucket][id]
		if ok {
			aggfields := agg.Fields()
			if fv > aggfields["min"].(float64) {
				nf["min"] = aggfields["min"]
			}
			if fv < aggfields["max"].(float64) {
				nf["max"] = aggfields["max"]
			}
			nf["sum"] = fv + aggfields["sum"].(float64)
			nf["count"] = aggfields["count"].(int64) + 1
		}

		na, _ := metric.New(name, m.Tags(), nf, tbucket)
		a.cache[tbucket][id] = &aggregate{na, true}
	}
}

func convert(in interface{}) (float64, bool) {
	switch v := in.(type) {
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float64:
		return v, true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

var invalidNameCharRE = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func sanitize(value string) string {
	return invalidNameCharRE.ReplaceAllString(value, "_")
}

func hashIDWithField(id uint64, fk string) uint64 {
	h := fnv.New64a()
	b := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(b, id)
	h.Write(b[:n])
	h.Write([]byte("\n"))
	h.Write([]byte(fk))
	h.Write([]byte("\n"))
	return h.Sum64()
}

// Push sends metrics to the output metric buffer
func (a *AzureMonitor) Push() []telegraf.Metric {
	var metrics []telegraf.Metric
	for tbucket, aggs := range a.cache {
		// Do not send metrics early
		if tbucket.After(time.Now().Add(-time.Minute)) {
			continue
		}
		for _, agg := range aggs {
			// Only send aggregates that have had an update since
			// the last push.
			if !agg.updated {
				continue
			}
			metrics = append(metrics, agg.Metric)
		}
	}
	return metrics
}

// Reset clears the cache of aggregate metrics
func (a *AzureMonitor) Reset() {
	for tbucket := range a.cache {
		// Remove aggregates older than 30 minutes
		if tbucket.Before(time.Now().Add(-time.Minute * 30)) {
			delete(a.cache, tbucket)
			continue
		}
		for id := range a.cache[tbucket] {
			a.cache[tbucket][id].updated = false
		}
	}
}

func init() {
	outputs.Add("azuremonitor", func() telegraf.Output {
		return &AzureMonitor{
			StringAsDimension: false,
			Timeout:           internal.Duration{Duration: time.Second * 5},
			cache:             make(map[time.Time]map[uint64]*aggregate, 36),
		}
	})
}