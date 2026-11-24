//nolint:testpackage // tests unexported handlers (viewJob, requestWithID, securityHeaders) directly
package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type mutableJobRepo struct {
	mockJobRepo
}

func (r *mutableJobRepo) Create(_ context.Context, job *Job) error {
	r.jobs = append([]Job{*job}, r.jobs...)

	return nil
}

func (r *mutableJobRepo) Delete(_ context.Context, id string) error {
	for i := range r.jobs {
		if r.jobs[i].ID == id {
			r.jobs = append(r.jobs[:i], r.jobs[i+1:]...)

			break
		}
	}

	return nil
}

func paginatedJobs(count int) []Job {
	jobs := make([]Job, count)

	for i := range jobs {
		jobs[i] = Job{
			ID:   fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1),
			Name: fmt.Sprintf("job-%d", i+1),
		}
	}

	return jobs
}

func newTestServer(t *testing.T, dir string) *Server {
	t.Helper()

	srv, err := New(NewService(nil, dir), ":0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return srv
}

func TestViewJobRendersPlaces(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-1111-1111-1111-111111111111"

	csv := "title,latitude,longitude\nPlace,1.5,2.5\n"
	if err := os.WriteFile(filepath.Join(dir, id+".csv"), []byte(csv), 0o600); err != nil {
		t.Fatalf("write csv: %v", err)
	}

	srv := newTestServer(t, dir)

	req := requestWithID(httptest.NewRequest(http.MethodGet, "/view?id="+id, http.NoBody))
	rec := httptest.NewRecorder()
	srv.viewJob(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{`id="map-modal"`, `initJobMap()`, `"title":"Place"`, `"latitude":1.5`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestViewJobEmptyState(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	id := "22222222-2222-2222-2222-222222222222"
	req := requestWithID(httptest.NewRequest(http.MethodGet, "/view?id="+id, http.NoBody))
	rec := httptest.NewRecorder()
	srv.viewJob(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "var places = [];") {
		t.Fatalf("expected empty places array, got:\n%s", body)
	}
}

func TestViewJobInvalidID(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	req := requestWithID(httptest.NewRequest(http.MethodGet, "/view?id=not-a-uuid", http.NoBody))
	rec := httptest.NewRecorder()
	srv.viewJob(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}

func TestSecurityHeadersAllowMapResources(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"tile.openstreetmap.org", "cdnjs.cloudflare.com"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP missing %q: %s", want, csp)
		}
	}
}

func newTestServerWithRepo(t *testing.T, repo JobRepository) *Server {
	t.Helper()

	srv, err := New(NewService(repo, t.TempDir()), ":0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return srv
}

func TestGetJobsHTMXTriggerIsPollingOnly(t *testing.T) {
	srv := newTestServerWithRepo(t, &mockJobRepo{jobs: paginatedJobs(1)})

	req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
	rec := httptest.NewRecorder()
	srv.getJobs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()

	// The returned tbody must only poll; it must NOT contain "load" in hx-trigger
	// because load on an outerHTML-swapped element would cause an immediate reload loop.
	if strings.Contains(body, `hx-trigger="load`) {
		t.Fatal("job_rows.html must not use hx-trigger=\"load\" — it causes an HTMX outerHTML reload loop")
	}

	if !strings.Contains(body, `hx-trigger="every 10s"`) {
		t.Fatal("job_rows.html must use hx-trigger=\"every 10s\" for periodic polling")
	}

	if !strings.Contains(body, `hx-delete="/delete?id=00000000-0000-0000-0000-000000000001&page=1"`) {
		t.Fatalf("delete request must preserve the current page: %s", body)
	}

	if !strings.Contains(body, `hx-target="#job-tbody"`) {
		t.Fatal("delete request must replace the paginated job body")
	}
}

func TestIndexScrapeFormReplacesPaginatedJobs(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rec := httptest.NewRecorder()
	srv.index(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `hx-target="#job-tbody"`) {
		t.Fatal("scrape form must target the paginated job body")
	}

	if !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Fatal("scrape form must replace the paginated job body")
	}
}

func TestScrapeRendersFirstPageAfterCreatingJob(t *testing.T) {
	repo := &mutableJobRepo{mockJobRepo: mockJobRepo{jobs: paginatedJobs(20)}}
	srv := newTestServerWithRepo(t, repo)

	form := url.Values{
		"name":      {"new-job"},
		"maxtime":   {"4m"},
		"keywords":  {"coffee"},
		"lang":      {"en"},
		"zoom":      {"15"},
		"radius":    {"10000"},
		"latitude":  {"0"},
		"longitude": {"0"},
		"depth":     {"10"},
	}
	req := httptest.NewRequest(http.MethodPost, "/scrape", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	srv.scrape(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, `<tbody id="job-tbody"`) {
		t.Fatal("scrape response must contain the complete paginated job body")
	}

	if !strings.Contains(body, "new-job") || !strings.Contains(body, "Page 1 of 2") {
		t.Fatalf("unexpected scrape response: %s", body)
	}

	if rows := strings.Count(body, "<tr>"); rows != 20 {
		t.Fatalf("got %d rows, want 20", rows)
	}
}

func TestDeleteRendersClampedCurrentPage(t *testing.T) {
	repo := &mutableJobRepo{mockJobRepo: mockJobRepo{jobs: paginatedJobs(21)}}
	srv := newTestServerWithRepo(t, repo)
	deletedJob := repo.jobs[20]

	req := httptest.NewRequest(
		http.MethodDelete,
		"/delete?id="+deletedJob.ID+"&page=2",
		http.NoBody,
	)
	req = requestWithID(req)
	rec := httptest.NewRecorder()
	srv.delete(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, `<tbody id="job-tbody"`) {
		t.Fatal("delete response must contain the complete paginated job body")
	}

	if strings.Contains(body, deletedJob.Name) {
		t.Fatalf("delete response still contains %q", deletedJob.Name)
	}

	if rows := strings.Count(body, "<tr>"); rows != 20 {
		t.Fatalf("got %d rows, want 20", rows)
	}
}

func TestIndexEnablesHTMXTemplateFragments(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rec := httptest.NewRecorder()
	srv.index(rec, req)

	body := rec.Body.String()

	// job_rows.html returns the <tbody> followed by an out-of-band <div>. Without template
	// fragments htmx 1.x parses that response inside a <table>, the HTML parser hoists the
	// <div> in front of it, and the swap replaces #job-tbody with the div's content.
	if !strings.Contains(body, `<meta name="htmx-config" content='{"useTemplateFragments":true}'>`) {
		t.Fatal("index.html must enable htmx template fragments, or the job list swap drops #job-tbody")
	}
}

func TestJobDataValidateWebhookURL(t *testing.T) {
	t.Parallel()

	validJob := func(webhookURL string) JobData {
		return JobData{
			Keywords:   []string{"plumbers in dallas"},
			Lang:       "en",
			Depth:      1,
			MaxTime:    time.Minute * 5,
			WebhookURL: webhookURL,
		}
	}

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "empty url allowed", url: "", wantErr: false},
		{name: "valid https url", url: "https://my-crm.com/api/webhook", wantErr: false},
		{name: "valid http url", url: "http://localhost:3000/webhook", wantErr: false},
		{name: "invalid scheme", url: "ftp://my-crm.com/api/webhook", wantErr: true},
		{name: "missing host", url: "https:///webhook", wantErr: true},
		{name: "invalid string", url: "not-a-valid-url", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobData := validJob(tt.url)
			err := jobData.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestApiScrapeWithWebhookURL(t *testing.T) {
	t.Parallel()

	repo := &mutableJobRepo{}
	srv := newTestServerWithRepo(t, repo)

	reqBody := `{
		"name": "Webhook Scrape",
		"keywords": ["coffee shops"],
		"lang": "en",
		"depth": 1,
		"max_time": 300,
		"webhook_url": "https://my-crm.com/webhook"
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.apiScrape(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	if len(repo.jobs) != 1 {
		t.Fatalf("expected 1 job created, got %d", len(repo.jobs))
	}

	created := repo.jobs[0]
	if created.Data.WebhookURL != "https://my-crm.com/webhook" {
		t.Errorf("webhook_url = %q, want %q", created.Data.WebhookURL, "https://my-crm.com/webhook")
	}
}

func TestAuthMiddleware(t *testing.T) {
	t.Parallel()

	repo := &mutableJobRepo{}
	svc := NewService(repo, t.TempDir())
	const secret = "test-secret-key-12345"

	srv, err := New(svc, ":0", secret)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 1. API request without auth -> 401 JSON
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", http.NoBody)
	rec := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing API key, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Unauthorized") {
		t.Fatalf("expected Unauthorized message in JSON body, got: %s", rec.Body.String())
	}

	// 2. API request with wrong X-API-Key -> 401 JSON
	req = httptest.NewRequest(http.MethodGet, "/api/v1/jobs", http.NoBody)
	req.Header.Set("X-API-Key", "wrong-key")
	rec = httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong API key, got %d", rec.Code)
	}

	// 3. API request with valid X-API-Key -> 200 OK
	req = httptest.NewRequest(http.MethodGet, "/api/v1/jobs", http.NoBody)
	req.Header.Set("X-API-Key", secret)
	rec = httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid X-API-Key, got %d", rec.Code)
	}

	// 4. API request with valid Authorization Bearer -> 200 OK
	req = httptest.NewRequest(http.MethodGet, "/api/v1/jobs", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec = httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid Bearer token, got %d", rec.Code)
	}

	// 5. Web UI request without auth -> 401 with WWW-Authenticate header
	req = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rec = httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated Web UI request, got %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("expected WWW-Authenticate header for Web UI request, got empty")
	}

	// 6. Web UI request with Basic Auth -> 200 OK
	req = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.SetBasicAuth("admin", secret)
	rec = httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for Basic Auth Web UI request, got %d", rec.Code)
	}

	// 7. Web UI request with cookie -> 200 OK
	req = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.AddCookie(&http.Cookie{Name: "api_key", Value: secret})
	rec = httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for cookie authenticated Web UI request, got %d", rec.Code)
	}

	// 8. Server without APIKey (unprotected mode) allows requests without auth
	openSrv, err := New(svc, ":0")
	if err != nil {
		t.Fatalf("New open server: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/jobs", http.NoBody)
	rec = httptest.NewRecorder()
	openSrv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for open server without API key, got %d", rec.Code)
	}
}
