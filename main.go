// job-status-to-apps-adapter
//
// This service periodically queries the DE database's job-status-updates table
// for new entries and propagates them up through the apps services's API, which
// eventually triggers job notifications in the UI.
//
// This service works by first querying for all jobs that have unpropagated
// statuses, iterating through each job and propagating all unpropagated
// status in the correct order. It records each attempt and will not re-attempt
// a propagation if the number of retries exceeds the configured maximum number
// of retries (which defaults to 3).
//
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	_ "expvar"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/cyverse-de/configurate"
	"github.com/cyverse-de/dbutil"
	"github.com/cyverse-de/go-mod/otelutils"
	"github.com/cyverse-de/version"
	_ "github.com/lib/pq"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "job-status-to-apps-adapter"
const otelName = "github.com/cyverse-de/job-status-to-apps-adapter"

var log = logrus.WithFields(logrus.Fields{"service": serviceName})
var httpClient = http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}

// JobStatusUpdate contains the data POSTed to the apps service.
type JobStatusUpdate struct {
	UUID string `json:"uuid"`
}

// Unpropagated returns a []string of the UUIDs for jobs that have steps that
// haven't been propagated yet but haven't passed their retry limit.
func Unpropagated(ctx context.Context, d *sql.DB, maxRetries int64) ([]string, error) {
	queryStr := `
	select distinct external_id
	  from job_status_updates
	 where propagated = 'false'
	   and propagation_attempts < $1`
	rows, err := d.QueryContext(ctx, queryStr, maxRetries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var retval []string
	for rows.Next() {
		var extID string
		err = rows.Scan(&extID)
		if err != nil {
			return nil, err
		}
		retval = append(retval, extID)
	}
	err = rows.Err()
	return retval, err
}

// Propagator looks for job status updates in the database and pushes them to
// the apps service if they haven't been successfully pushed there yet.
type Propagator struct {
	db      *sql.DB
	appsURI string
}

// NewPropagator returns a *Propagator that has been initialized with a new
// transaction.
func NewPropagator(d *sql.DB, appsURI string) (*Propagator, error) {
	var err error
	if err != nil {
		return nil, err
	}
	return &Propagator{
		db:      d,
		appsURI: appsURI,
	}, nil
}

// Propagate pushes the update to the apps service.
func (p *Propagator) Propagate(ctx context.Context, uuid string) error {
	jsu := JobStatusUpdate{
		UUID: uuid,
	}

	log.Infof("Job status in the propagate function for job %s is: %#v", jsu.UUID, jsu)
	msg, err := json.Marshal(jsu)
	if err != nil {
		log.Error(err)
		return err
	}

	buf := bytes.NewBuffer(msg)
	if err != nil {
		log.Error(err)
		return err
	}

	log.Infof("Message to propagate: %s", string(msg))

	log.Infof("Sending job status to %s in the propagate function for job %s", p.appsURI, jsu.UUID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.appsURI, buf)
	if err != nil {
		log.Errorf("Error sending job status to %s in the propagate function for job %s: %#v", p.appsURI, jsu.UUID, err)
		return err
	}

	req.Header.Set("content-type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Errorf("Error sending job status to %s in the propagate function for job %s: %#v", p.appsURI, jsu.UUID, err)
		return err
	}
	defer resp.Body.Close()

	log.Infof("Response from %s in the propagate function for job %s is: %s", p.appsURI, jsu.UUID, resp.Status)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errors.New("bad response")
	}

	return nil
}

func main() {
	var (
		cfgPath     = flag.String("config", "", "Path to the config file. Required.")
		showVersion = flag.Bool("version", false, "Print the version information")
		dbURI       = flag.String("db", "", "The URI used to connect to the database")
		maxRetries  = flag.Int64("retries", 3, "The maximum number of propagation retries to make")
		batchSize   = flag.Int("batch-size", 1000, "The number of concurrent jobs to process.")
		err         error
		cfg         *viper.Viper
		db          *sql.DB
		appsURI     string
	)

	var tracerCtx, cancel = context.WithCancel(context.Background())
	defer cancel()
	shutdown := otelutils.TracerProviderFromEnv(tracerCtx, serviceName, func(e error) { log.Fatal(e) })
	defer shutdown()

	flag.Parse()

	if *showVersion {
		version.AppVersion()
		os.Exit(0)
	}

	if *cfgPath == "" {
		fmt.Println("Error: --config must be set.")
		flag.PrintDefaults()
		os.Exit(-1)
	}

	cfg, err = configurate.InitDefaults(*cfgPath, configurate.JobServicesDefaults)
	if err != nil {
		log.Error(err)
		os.Exit(-1)
	}

	log.Info("Done reading config.")

	if *dbURI == "" {
		*dbURI = cfg.GetString("db.uri")
	} else {
		cfg.Set("db.uri", *dbURI)
	}

	appsURI = cfg.GetString("apps.callbacks_uri")

	log.Info("Connecting to the database...")
	connector, err := dbutil.NewDefaultConnector("1m")
	if err != nil {
		log.Fatal(err)
	}

	db, err = connector.Connect("postgres", *dbURI)
	if err != nil {
		log.Fatal(err)
	}

	if err = db.Ping(); err != nil {
		log.Fatal(err)
	}
	log.Info("Connected to the database")

	go func() {
		sock, err := net.Listen("tcp", "0.0.0.0:60000")
		if err != nil {
			log.Fatal(err)
		}
		err = http.Serve(sock, nil)
		if err != nil {
			log.Fatal(err)
		}
	}()

	for {
		ctx, span := otel.Tracer(otelName).Start(context.Background(), "propagation loop")
		var batches [][]string
		var wg sync.WaitGroup

		unpropped, err := Unpropagated(ctx, db, *maxRetries)
		if err != nil {
			span.End()
			log.Fatal(err)
		}

		for *batchSize < len(unpropped) {
			unpropped, batches = unpropped[*batchSize:], append(batches, unpropped[0:*batchSize])
		}
		batches = append(batches, unpropped)

		for _, batch := range batches {
			for _, jobExtID := range batch {
				wg.Add(1)

				go func(ctx context.Context, db *sql.DB, maxRetries int64, appsURI string, jobExtID string) {
					defer wg.Done()
					separatedSpanContext := trace.SpanContextFromContext(ctx)
					outerCtx := trace.ContextWithSpanContext(context.Background(), separatedSpanContext)

					ctx, span := otel.Tracer(otelName).Start(outerCtx, "propagator goroutine")
					defer span.End()

					proper, err := NewPropagator(db, appsURI)
					if err != nil {
						log.Error(err)
					}

					if err = proper.Propagate(ctx, jobExtID); err != nil {
						log.Error(err)
					}

				}(ctx, db, *maxRetries, appsURI, jobExtID)
			}

			wg.Wait()
		}

		span.End()
	}
}
