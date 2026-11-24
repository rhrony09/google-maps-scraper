package webrunner

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gosom/google-maps-scraper/deduper"
	"github.com/gosom/google-maps-scraper/exiter"
	"github.com/gosom/google-maps-scraper/runner"
	"github.com/gosom/google-maps-scraper/tlmt"
	"github.com/gosom/google-maps-scraper/web"
	"github.com/gosom/google-maps-scraper/web/sqlite"
	"github.com/gosom/scrapemate"
	"github.com/gosom/scrapemate/adapters/writers/csvwriter"
	"github.com/gosom/scrapemate/scrapemateapp"
	"golang.org/x/sync/errgroup"
)

type webrunner struct {
	srv           *web.Server
	svc           *web.Service
	cfg           *runner.Config
	setupMate     func(context.Context, io.Writer, *web.Job) (mateRunner, error)
	client        *http.Client
	onWebhookSent func(job *web.Job, err error)
}

type mateRunner interface {
	Start(context.Context, ...scrapemate.IJob) error
	Close() error
}

func New(cfg *runner.Config) (runner.Runner, error) {
	if cfg.DataFolder == "" {
		return nil, fmt.Errorf("data folder is required")
	}

	if err := os.MkdirAll(cfg.DataFolder, os.ModePerm); err != nil {
		return nil, err
	}

	const dbfname = "jobs.db"

	dbpath := filepath.Join(cfg.DataFolder, dbfname)

	repo, err := sqlite.New(dbpath)
	if err != nil {
		return nil, err
	}

	svc := web.NewService(repo, cfg.DataFolder)

	srv, err := web.New(svc, cfg.Addr, cfg.APIKey)
	if err != nil {
		return nil, err
	}

	ans := webrunner{
		srv:       srv,
		svc:       svc,
		cfg:       cfg,
		setupMate: defaultSetupMate(cfg),
		client:    &http.Client{Timeout: 30 * time.Second},
	}

	return &ans, nil
}

func (w *webrunner) Run(ctx context.Context) error {
	egroup, ctx := errgroup.WithContext(ctx)

	egroup.Go(func() error {
		return w.work(ctx)
	})

	egroup.Go(func() error {
		return w.srv.Start(ctx)
	})

	return egroup.Wait()
}

func (w *webrunner) Close(context.Context) error {
	return nil
}

func (w *webrunner) work(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			jobs, err := w.svc.SelectPending(ctx)
			if err != nil {
				return err
			}

			for i := range jobs {
				select {
				case <-ctx.Done():
					return nil
				default:
					t0 := time.Now().UTC()
					if err := w.scrapeJob(ctx, &jobs[i]); err != nil {
						params := map[string]any{
							"job_count": len(jobs[i].Data.Keywords),
							"duration":  time.Now().UTC().Sub(t0).String(),
							"error":     err.Error(),
						}

						evt := tlmt.NewEvent("web_runner", params)

						_ = runner.Telemetry().Send(ctx, evt)

						log.Printf("error scraping job %s: %v", jobs[i].ID, err)
					} else {
						params := map[string]any{
							"job_count": len(jobs[i].Data.Keywords),
							"duration":  time.Now().UTC().Sub(t0).String(),
						}

						_ = runner.Telemetry().Send(ctx, tlmt.NewEvent("web_runner", params))

						log.Printf("job %s scraped successfully", jobs[i].ID)
					}
				}
			}
		}
	}
}

func (w *webrunner) scrapeJob(ctx context.Context, job *web.Job) error {
	job.Status = web.StatusWorking

	err := w.svc.Update(ctx, job)
	if err != nil {
		return err
	}

	if len(job.Data.Keywords) == 0 {
		job.Status = web.StatusFailed
		err = errors.New("missing keywords")
		w.dispatchWebhook(job, err)

		return w.svc.Update(ctx, job)
	}

	outpath := filepath.Join(w.cfg.DataFolder, job.ID+".csv")

	outfile, err := os.Create(outpath)
	if err != nil {
		job.Status = web.StatusFailed
		err2 := w.svc.Update(ctx, job)
		if err2 != nil {
			log.Printf("failed to update job status: %v", err2)
		}
		w.dispatchWebhook(job, err)

		return err
	}

	defer func() {
		_ = outfile.Close()
	}()

	setupMate := w.setupMate
	if setupMate == nil {
		setupMate = defaultSetupMate(w.cfg)
	}

	mate, err := setupMate(ctx, outfile, job)
	if err != nil {
		job.Status = web.StatusFailed

		err2 := w.svc.Update(ctx, job)
		if err2 != nil {
			log.Printf("failed to update job status: %v", err2)
		}
		w.dispatchWebhook(job, err)

		return err
	}

	defer mate.Close()

	var coords string
	if job.Data.Lat != "" && job.Data.Lon != "" {
		coords = job.Data.Lat + "," + job.Data.Lon
	}

	dedup := deduper.New()
	exitMonitor := exiter.New()

	seedJobs, err := runner.CreateSeedJobs(
		job.Data.FastMode,
		job.Data.Lang,
		strings.NewReader(strings.Join(job.Data.Keywords, "\n")),
		job.Data.Depth,
		job.Data.Email,
		coords,
		job.Data.Zoom,
		func() float64 {
			if job.Data.Radius <= 0 {
				return 10000 // 10 km
			}

			return float64(job.Data.Radius)
		}(),
		dedup,
		exitMonitor,
		w.cfg.ExtraReviews || job.Data.ExtraReviews,
	)
	if err != nil {
		job.Status = web.StatusFailed
		err2 := w.svc.Update(ctx, job)
		if err2 != nil {
			log.Printf("failed to update job status: %v", err2)
		}
		w.dispatchWebhook(job, err)

		return err
	}

	if len(seedJobs) > 0 {
		exitMonitor.SetSeedCount(len(seedJobs))

		allowedSeconds := max(60, len(seedJobs)*10*job.Data.Depth/50+120)

		if job.Data.MaxTime > 0 {
			if job.Data.MaxTime.Seconds() < 180 {
				allowedSeconds = 180
			} else {
				allowedSeconds = int(job.Data.MaxTime.Seconds())
			}
		}

		log.Printf("running job %s with %d seed jobs and %d allowed seconds", job.ID, len(seedJobs), allowedSeconds)

		mateCtx, cancel := context.WithTimeout(ctx, time.Duration(allowedSeconds)*time.Second)
		defer cancel()

		exitMonitor.SetCancelFunc(cancel)

		go exitMonitor.Run(mateCtx)

		err = mate.Start(mateCtx, seedJobs...)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			cancel()

			job.Status = web.StatusFailed
			err2 := w.svc.Update(ctx, job)
			if err2 != nil {
				log.Printf("failed to update job status: %v", err2)
			}
			w.dispatchWebhook(job, err)

			return err
		}

		cancel()
	}

	job.Status = web.StatusOK
	if err := w.svc.Update(ctx, job); err != nil {
		return err
	}

	w.dispatchWebhook(job, nil)

	return nil
}

func defaultSetupMate(cfg *runner.Config) func(context.Context, io.Writer, *web.Job) (mateRunner, error) {
	return func(_ context.Context, writer io.Writer, job *web.Job) (mateRunner, error) {
		opts := []func(*scrapemateapp.Config) error{
			scrapemateapp.WithConcurrency(cfg.Concurrency),
			scrapemateapp.WithExitOnInactivity(time.Minute * 3),
		}

		if !job.Data.FastMode {
			opts = append(opts,
				scrapemateapp.WithJS(scrapemateapp.DisableImages()),
			)
		} else {
			opts = append(opts,
				scrapemateapp.WithStealth("firefox"),
			)
		}

		opts = runner.AppendBrowserCapacityOptions(opts, cfg)

		hasProxy := false

		if len(cfg.Proxies) > 0 {
			opts = append(opts, scrapemateapp.WithProxies(cfg.Proxies))
			hasProxy = true
		} else if len(job.Data.Proxies) > 0 {
			opts = append(opts,
				scrapemateapp.WithProxies(job.Data.Proxies),
			)
			hasProxy = true
		}

		if !cfg.DisablePageReuse {
			opts = append(opts,
				scrapemateapp.WithPageReuseLimit(2),
				scrapemateapp.WithBrowserReuseLimit(200),
			)
		}

		log.Printf("job %s has proxy: %v", job.ID, hasProxy)

		csvWriter := csvwriter.NewCsvWriter(csv.NewWriter(writer))

		writers := []scrapemate.ResultWriter{csvWriter}

		matecfg, err := scrapemateapp.NewConfig(
			writers,
			opts...,
		)
		if err != nil {
			return nil, err
		}

		return scrapemateapp.NewScrapeMateApp(matecfg)
	}
}

const (
	webhookTimeout   = 30 * time.Second
	webhookUserAgent = "google-maps-scraper/webhook"
)

// WebhookPayload represents the JSON payload dispatched to a webhook URL upon job completion or failure.
type WebhookPayload struct {
	Event       string      `json:"event"`
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Status      string      `json:"status"`
	PlacesCount int         `json:"places_count"`
	DownloadURL string      `json:"download_url,omitempty"`
	Error       string      `json:"error,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	Data        web.JobData `json:"data"`
}

func (w *webrunner) sendWebhook(ctx context.Context, job *web.Job, jobErr error) error {
	if job.Data.WebhookURL == "" {
		return nil
	}

	payload := WebhookPayload{
		ID:        job.ID,
		Name:      job.Name,
		Status:    job.Status,
		CreatedAt: job.Date,
		Data:      job.Data,
	}

	if job.Status == web.StatusOK {
		payload.Event = "job.completed"
		payload.DownloadURL = "/api/v1/jobs/" + job.ID + "/download"

		places, err := w.svc.GetPlaces(ctx, job.ID)
		if err == nil {
			payload.PlacesCount = len(places)
		}
	} else {
		payload.Event = "job.failed"
		if jobErr != nil {
			payload.Error = jobErr.Error()
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("webhook: failed to marshal payload for job %s: %v", job.ID, err)

		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	client := w.client
	if client == nil {
		client = &http.Client{Timeout: webhookTimeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.Data.WebhookURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("webhook: failed to create request for job %s: %v", job.ID, err)

		return fmt.Errorf("create webhook request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", webhookUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("webhook: failed to deliver to %s for job %s: %v", job.Data.WebhookURL, job.ID, err)

		return fmt.Errorf("deliver webhook: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		log.Printf("webhook: %s returned non-2xx status code %d for job %s", job.Data.WebhookURL, resp.StatusCode, job.ID)

		return fmt.Errorf("webhook endpoint returned status %d", resp.StatusCode)
	}

	log.Printf("webhook: delivered to %s for job %s (status %d)", job.Data.WebhookURL, job.ID, resp.StatusCode)

	return nil
}

func (w *webrunner) dispatchWebhook(job *web.Job, jobErr error) {
	if job.Data.WebhookURL == "" {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
		defer cancel()

		err := w.sendWebhook(ctx, job, jobErr)
		if w.onWebhookSent != nil {
			w.onWebhookSent(job, err)
		}
	}()
}
