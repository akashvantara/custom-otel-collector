package cloudeventexporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

var (
	filters        []string         // k8s.event.reason filters
	filterAllowAll bool     = false // if configuration changes this to true, it'll let pass all of the logs

	typeVersion string // typeverson will define the body type of CloudEvent (right now it's v1 specific)
)

const (
	// Cloud-event body skeleton
	CE_DATA_META_BODY = `{"reason":"%s","start_time":"%s","name":"%s","namespace":"%s","count":%d,"message":"%s"}`

	// Cloud-event required headers
	HEADER_CE_ID          = "Ce-Id"
	HEADER_CE_TYPE        = "Ce-Type"
	HEADER_CE_SOURCE      = "Ce-Source"
	HEADER_CE_SPECVERSION = "Ce-Specversion"
	HEADER_CONTENT_TYPE   = "Content-Type"

	// Other required HTTP headers
	HEADER_RETRY_AFTER = "Retry-After"
	CONTENT_TYPE       = "application/json"

	// Open-telemetry required resources to look for in logs
	ATTR_EVENT_COUNT      = "k8s.event.count"
	ATTR_EVENT_NAME       = "k8s.event.name"
	ATTR_EVENT_NS         = "k8s.namespace.name"
	ATTR_EVENT_REASON     = "k8s.event.reason"
	ATTR_EVENT_START_TIME = "k8s.event.start_time"
	ATTR_EVENT_UID        = "k8s.event.uid"

	// Channel size and also the concurrent go thread counts which
	// reads gets the cloud-event and sends HTTP request
	CHAN_SZ = 2

	// To avoid fetching attribute from OTel use FETCH_ATTR = false
	FETCH_ATTR    = true

	// Enable retry for failed messages
	RETRY_ENABLED = false
)

type cloudeventTransformExporter struct {
	config      *Config
	client      *http.Client
	logger      *zap.Logger
	settings    component.TelemetrySettings
	useragent   string
	source      string
	specversion string
	ceChan      chan *cloudeventdata
}

type cloudeventdata struct {
	count     int
	message   string
	name      string
	namespace string
	reason    string
	startTime string
	uid       string // This field will be converted and passed to cloudeventTransformExporter.id
}

// Create new exporter.
func newExporter(cf component.Config, set exporter.CreateSettings) (*cloudeventTransformExporter, error) {
	conf := cf.(*Config)
	var err error = nil

	if err = conf.Validate(); err != nil {
		return nil, err
	}

	if len(conf.Ce.SpecVersion) > 0 {
		typeVersion = "v" + string(conf.Ce.SpecVersion[0])
	}

	if len(conf.Filter) > 0 {
		filters = strings.Split(conf.Filter, "|")
		filtersLen := len(filters)

		if filtersLen == 0 {
			err = errors.New(
				fmt.Sprintf("some valid fields should be provided in filters ('*', 'Created|Deleted'), provided: %s",
					conf.Filter,
				),
			)
		} else if filtersLen == 1 && filters[0] == "*" {
			filterAllowAll = true
		}
	}

	userAgent := fmt.Sprintf("%s/%s (%s/%s)",
		set.BuildInfo.Description, set.BuildInfo.Version, runtime.GOOS, runtime.GOARCH)

	// client construction is deferred to start
	return &cloudeventTransformExporter{
		config:    conf,
		logger:    set.Logger,
		useragent: userAgent,
		source:    conf.Ce.Source,
		ceChan:    make(chan *cloudeventdata, CHAN_SZ),
		settings:  set.TelemetrySettings,
	}, nil
}

// start actually creates the HTTP client. The client construction is deferred till this point as this
// is the only place we get hold of Extensions which are required to construct auth round tripper.
func (e *cloudeventTransformExporter) start(_ context.Context, host component.Host) error {
	client, err := e.config.HTTPClientSettings.ToClient(host, e.settings)
	if err != nil {
		return err
	}
	e.client = client

	// Spin the go-routines which will listen to messages dropped in ceChan channel
	for i := 0; i < CHAN_SZ; i++ {
		go e.exportMessage()
	}
	return nil
}

func (e *cloudeventTransformExporter) shutdown(_ context.Context) error {
	// Close the channel to receive messages further
	close(e.ceChan)
	return nil
}

func (e *cloudeventTransformExporter) pushLogs(ctx context.Context, ld plog.Logs) error {
	// Body that can be re-used for all the messages present in loop
	// to avoid extra allocation/s
	var ce cloudeventdata

	// Remove anything not required from logs
	if !filterAllowAll {
		ld.ResourceLogs().RemoveIf(func(rl plog.ResourceLogs) bool {
			rl.ScopeLogs().RemoveIf(func(sl plog.ScopeLogs) bool {
				sl.LogRecords().RemoveIf(func(lr plog.LogRecord) bool {
					reason, reasonOk := lr.Attributes().Get(ATTR_EVENT_REASON)
					if !reasonOk {
						return false
					}

					reasonFound := false
					for _, r := range filters {
						if r == reason.AsString() {
							reasonFound = true
							break
						}
					}

					return !reasonFound
				})
				return sl.LogRecords().Len() == 0
			})
			return rl.ScopeLogs().Len() == 0
		})
	}

	// Convert the log/s
	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		scopeLogs := ld.ResourceLogs().At(i).ScopeLogs()

		for j := 0; j < scopeLogs.Len(); j++ {
			logRecord := scopeLogs.At(j)
			records := logRecord.LogRecords()

			for k := 0; k < records.Len(); k++ {
				//var cloudEventMetaData string
				currentMessage := records.At(k).Body()

				// Get all the required attributes
				if FETCH_ATTR {
					attrMap := records.At(k).Attributes()

					// Check if the required things are present,
					// if not fail at the earliest reporting missing things
					eventCount, eventCountOk := attrMap.Get(ATTR_EVENT_COUNT)
					eventName, eventNameOk := attrMap.Get(ATTR_EVENT_NAME)
					eventNs, eventNsOk := attrMap.Get(ATTR_EVENT_NS)
					eventUid, eventUidOk := attrMap.Get(ATTR_EVENT_UID)
					reason, reasonOk := attrMap.Get(ATTR_EVENT_REASON)
					startTime, startTimeOk := attrMap.Get(ATTR_EVENT_START_TIME)

					anyError := !(reasonOk && startTimeOk && eventNameOk && eventUidOk && eventNsOk && eventCountOk)

					if anyError {
						overAllErrStr := ""

						if !reasonOk {
							overAllErrStr += "{" + ATTR_EVENT_REASON + "} "
						}

						if !startTimeOk {
							overAllErrStr += "{" + ATTR_EVENT_START_TIME + "} "
						}

						if !eventNameOk {
							overAllErrStr += "{" + ATTR_EVENT_NAME + "} "
						}

						if !eventUidOk {
							overAllErrStr += "{" + ATTR_EVENT_UID + "} "
						}

						if !eventNsOk {
							overAllErrStr += "{" + ATTR_EVENT_NS + "} "
						}

						if !eventCountOk {
							overAllErrStr += "{" + ATTR_EVENT_COUNT + "} "
						}

						return errors.New(fmt.Sprintf("Couldn't find %sattributes in the log", overAllErrStr))
					}

					ce = cloudeventdata{
						count:     int(eventCount.Int()),
						message:   currentMessage.AsString(),
						name:      eventName.AsString(),
						namespace: eventNs.AsString(),
						reason:    reason.AsString(),
						startTime: startTime.AsString(),
						uid:       eventUid.AsString(),
					}
				} else {
					// Useful case for testing but this can be totally removed
					// Though it can be utilized if expansion is required later
					ce = cloudeventdata{
						count:     0,
						message:   currentMessage.AsString(),
						name:      "name",
						namespace: "ns",
						reason:    "TestReason",
						startTime: "",
						uid:       "fhapohnea-afj-ajfa",
					}
				}

				// Send the message to channel so that it can be processed in parallel
				e.ceChan <- &ce
			}
		}
	}

	return nil
}

func (e *cloudeventTransformExporter) exportMessage() {
	for ce := range e.ceChan {
		// Correct JSON message if it has quotes
		msg := strings.ReplaceAll(ce.message, "\"", "\\\"")

		// Prepare JSON body
		json_body := fmt.Sprintf(CE_DATA_META_BODY,
			ce.reason,
			ce.startTime,
			ce.name,
			ce.namespace,
			ce.count,
			msg,
		)

		// Create new request body and configure it with required things
		req, err := http.NewRequest(http.MethodPost, e.config.Endpoint, bytes.NewReader([]byte(json_body)))

		if err != nil {
			e.logger.Error(err.Error())
			continue
		}

		// Add all the required headers
		req.Header.Add(HEADER_CE_ID, ce.uid)
		req.Header.Add(HEADER_CE_TYPE, configureCeType(e.config.Ce.AppendType, ce.reason))
		req.Header.Add(HEADER_CE_SOURCE, e.config.Ce.Source)
		req.Header.Add(HEADER_CE_SPECVERSION, e.config.Ce.SpecVersion)
		req.Header.Add(HEADER_CONTENT_TYPE, CONTENT_TYPE)

		res, err := e.client.Do(req)

		if err != nil {
			e.logger.Error(err.Error())
			continue
		}

		// Check if the status code is acceptable and continue for next requests
		if res.StatusCode >= 200 && res.StatusCode <= 299 {
			continue
		}

		var formattedErr error = fmt.Errorf("error exporting items, request to %s responded with HTTP Status Code %d",
			e.config.Endpoint, res.StatusCode)

		// If enabled, retry for errors, otherwise print error and leave
		if RETRY_ENABLED {
			retryAfter := 0

			// Check if the server is overwhelmed.
			// See spec https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/protocol/otlp.md#otlphttp-throttling
			isThrottleError := res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable
			if val := res.Header.Get(HEADER_RETRY_AFTER); isThrottleError && val != "" {
				if seconds, err2 := strconv.Atoi(val); err2 == nil {
					retryAfter = seconds
				}
			}
			err = exporterhelper.NewThrottleRetry(formattedErr, time.Duration(retryAfter)*time.Second)
			e.logger.Error(err.Error())
			continue
		}

		e.logger.Error(formattedErr.Error())
	}
}

// Configures Ce-Type header's value, using the given reason (removes any spaces present)
func configureCeType(pretext string, reason string) string {
	var ret strings.Builder
	ret.Grow(len(pretext) + len(reason))

	ret.WriteString(pretext)
	ret.WriteRune('.')
	ret.WriteString(typeVersion) // It'll define the version
	ret.WriteRune('.')

	for _, ch := range reason {
		if !unicode.IsSpace(ch) {
			ret.WriteRune(ch)
		}
	}

	return ret.String()
}
