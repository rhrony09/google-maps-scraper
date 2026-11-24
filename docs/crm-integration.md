# CRM & Backend Integration Guide (API + Webhook)

This guide explains how to connect your CRM or external application to **Google Maps Scraper** using the local Docker service, REST API, and automated Webhooks.

---

## Table of Contents

1. [Architecture & Workflow](#1-architecture--workflow)
2. [Running Scraper as a Daemon (Docker)](#2-running-scraper-as-a-daemon-docker)
3. [API Reference: Create Scraping Job](#3-api-reference-create-scraping-job)
4. [Immediate API Response](#4-immediate-api-response)
5. [Webhook Specification](#5-webhook-specification)
6. [Scraped Data Points (36 CSV Columns)](#6-scraped-data-points-36-csv-columns)
7. [Recommended CRM Field Mapping](#7-recommended-crm-field-mapping)
8. [End-to-End Implementation Examples](#8-end-to-end-implementation-examples)
   - [PHP / Laravel Example](#php--laravel-example)
   - [Node.js / Express Example](#nodejs--express-example)

---

## 1. Architecture & Workflow

```
+------------------+         1. POST /api/v1/jobs          +----------------------+
|                  | ------------------------------------> |                      |
|                  | <------------------------------------ |                      |
|                  |          Response: { "id": "..." }    | Google Maps Scraper  |
|  CRM Application |                                       |    (Docker Daemon)   |
| (PHP/Node/Python)|        2. Background Webhook Event    |   - Headless Chrome  |
|                  | <------------------------------------ |   - SQLite DB        |
|                  |       POST {webhook_url} (on finish)  |                      |
|                  |                                       |                      |
|                  |    3. GET /api/v1/jobs/{id}/download  |                      |
|                  | ------------------------------------> |                      |
|                  | <------------------------------------ |                      |
+------------------+          CSV Stream (36 Fields)       +----------------------+
        |
        v
[Parse CSV -> Deduplicate by place_id -> Insert Leads into CRM DB]
```

### Why use Webhooks instead of Polling?
- **Zero Latency:** Your CRM is notified within milliseconds after Chromium finishes writing results.
- **No Resource Waste:** No need to run tight polling loops (`sleep(10)`) or repetitive database queries.
- **Reliable & Non-blocking:** Scraping jobs can run for 3 minutes to an hour without holding any HTTP connections open.

---

## 2. Running Scraper as a Daemon (Docker)

Use `docker-compose.yml` to keep the scraper continuously running in the background. If the container crashes or the system reboots, Docker automatically restarts it (`restart: unless-stopped`).

### `docker-compose.yml`
```yaml
services:
  gmaps-scraper:
    build:
      context: .
      dockerfile: Dockerfile
    container_name: gmaps-custom-scraper
    restart: unless-stopped
    ports:
      - "8080:8080"
    environment:
      # Protect API and Web UI with this secret key
      - API_KEY=${API_KEY:-my-secure-api-key}
    volumes:
      - ./gmapsdata:/gmapsdata
    command: ["-data-folder", "/gmapsdata"]
```

### Start the Service:
```bash
docker compose up --build -d
```

- **Web Dashboard:** [http://localhost:8080](http://localhost:8080)
- **Interactive OpenAPI / Swagger Docs:** [http://localhost:8080/api/docs](http://localhost:8080/api/docs)

---

## 3. API Reference: Create Scraping Job

Send a `POST` request to start a new scraping task.

- **Method:** `POST`
- **URL:** `http://localhost:8080/api/v1/jobs`
- **Headers:** 
  - `Content-Type: application/json`
  - `X-API-Key: <your-api-key>` *(or `Authorization: Bearer <your-api-key>`)*

> [!NOTE]
> If `API_KEY` is configured on the server, all requests without a valid `X-API-Key` or `Authorization` header will receive `401 Unauthorized`. Browser visits to the Web UI will prompt for HTTP Basic Auth (enter your API key as password).

### Request Body (JSON)

```json
{
  "name": "Dentists in Berlin",
  "keywords": [
    "dentists in berlin"
  ],
  "lang": "en",
  "depth": 10,
  "max_time": 300,
  "zoom": 15,
  "radius": 10000,
  "fast_mode": false,
  "email": true,
  "extra_reviews": false,
  "proxies": [],
  "webhook_url": "https://my-crm.com/api/webhooks/google-maps"
}
```

### Parameter Reference

| Parameter | Type | Required? | Default | Description & Constraints |
|---|---|---|---|---|
| `name` | string | **Required** | - | A descriptive title for the job in your CRM records. |
| `keywords` | array of strings | **Required** | - | Search query array (e.g. `["dentists in berlin", "orthodontists in berlin"]`). |
| `lang` | string | **Required** | - | 2-letter language code (e.g., `"en"`, `"de"`, `"bn"`). Must be exactly 2 characters. |
| `depth` | integer | **Required** | - | Maximum scroll depth. `1` yields ~20 results, `5` yields ~100 results, etc. |
| `max_time` | integer | **Required** | - | Maximum runtime in **seconds**. Minimum allowed is `180` (3 minutes). Example: `300` for 5 minutes. |
| `email` | boolean | Optional | `false` | When `true`, visits official websites of extracted places to collect contact emails. |
| `webhook_url` | string | Optional | `""` | HTTP/HTTPS URL where the completion notification will be delivered. |
| `radius` | integer | Optional | `10000` | Search radius in **meters** around coordinates (default: 10,000m = 10km). |
| `zoom` | integer | Optional | `15` | Map zoom level from `0` to `21`. |
| `lat` | string | Optional | `""` | Search center Latitude (e.g., `"52.5200"`). Required if `fast_mode` is `true`. |
| `lon` | string | Optional | `""` | Search center Longitude (e.g., `"13.4050"`). Required if `fast_mode` is `true`. |
| `fast_mode` | boolean | Optional | `false` | Beta fast mode (up to 21 results by distance). Requires `lat` and `lon`. |
| `extra_reviews` | boolean | Optional | `false` | When `true`, scrapes up to ~300 extended reviews per listing. |
| `proxies` | array of strings | Optional | `[]` | List of proxies (e.g., `["http://user:pass@proxy1:8080", "socks5://proxy2:1080"]`). |

---

## 4. Immediate API Response

When a job is successfully scheduled, the server immediately returns HTTP 201:

- **Status:** `201 Created`
- **Body:**
```json
{
  "id": "e6f47738-953e-46cb-8b5e-2b5e28a598c1"
}
```

Save this `id` in your CRM database with a status such as `pending` or `in_progress`.

---

## 5. Webhook Specification

Once scraping completes or encounters a fatal error, the scraper dispatches an `HTTP POST` request to your `webhook_url`.

- **Method:** `POST`
- **Headers:**
  - `Content-Type: application/json`
  - `User-Agent: google-maps-scraper/webhook`
- **Timeout:** 30 seconds

### Success Payload (`event: "job.completed"`)

```json
{
  "event": "job.completed",
  "id": "e6f47738-953e-46cb-8b5e-2b5e28a598c1",
  "name": "Dentists in Berlin",
  "status": "ok",
  "places_count": 24,
  "download_url": "/api/v1/jobs/e6f47738-953e-46cb-8b5e-2b5e28a598c1/download",
  "error": "",
  "created_at": "2026-11-12T13:45:00Z",
  "data": {
    "keywords": ["dentists in berlin"],
    "lang": "en",
    "depth": 1,
    "max_time": 300000000000,
    "email": true,
    "zoom": 15,
    "radius": 10000,
    "webhook_url": "https://my-crm.com/api/webhooks/google-maps"
  }
}
```

### Failure Payload (`event: "job.failed"`)

```json
{
  "event": "job.failed",
  "id": "e6f47738-953e-46cb-8b5e-2b5e28a598c1",
  "name": "Dentists in Berlin",
  "status": "failed",
  "places_count": 0,
  "download_url": "",
  "error": "failed to launch browser context: timeout",
  "created_at": "2026-11-12T13:45:00Z",
  "data": {
    "keywords": ["dentists in berlin"],
    "lang": "en",
    "depth": 1,
    "webhook_url": "https://my-crm.com/api/webhooks/google-maps"
  }
}
```

---

## 6. Scraped Data Points (36 CSV Columns)

To download the full dataset, call:
`GET http://localhost:8080/api/v1/jobs/{id}/download`

The response is a standard CSV file with the following 36 columns:

| # | Column Header | Data Type | Description & Example |
|---|---|---|---|
| 1 | `input_id` | string | Internal query identifier or custom ID from query. |
| 2 | `link` | string | Full Google Maps URL for the business listing. |
| 3 | `title` | string | Official business or organization name. |
| 4 | `category` | string | Primary category (e.g. `Dental clinic`, `Restaurant`). |
| 5 | `address` | string | Street address or short location. |
| 6 | `open_hours` | string (JSON) | Operating hours per weekday (e.g. `{"Monday":"09:00-18:00",...}`). |
| 7 | `popular_times` | string (JSON) | Hourly visitor traffic pattern distribution. |
| 8 | `website` | string | Official business website URL. |
| 9 | `phone` | string | Business phone number in local or international format. |
| 10 | `plus_code` | string | Google Plus Code (e.g., `9F42VFR8+39`). |
| 11 | `review_count` | integer | Total count of published customer reviews. |
| 12 | `review_rating` | float | Average Google star rating (e.g., `4.8`). |
| 13 | `reviews_per_rating` | string (JSON) | Breakdown of review counts for 1, 2, 3, 4, and 5 stars. |
| 14 | `latitude` | float | GPS Latitude (e.g., `52.5200065`). |
| 15 | `longitude` | float | GPS Longitude (e.g., `13.4049540`). |
| 16 | `cid` | string | Google's unique Customer ID (CID). |
| 17 | `status` | string | Status: `OPERATIONAL`, `CLOSED_TEMPORARILY`, `CLOSED_PERMANENTLY`. |
| 18 | `descriptions` | string | Editorial summary or tagline. |
| 19 | `reviews_link` | string | Direct link to read user reviews on Google. |
| 20 | `thumbnail` | string | Primary thumbnail / cover image URL. |
| 21 | `timezone` | string | Local timezone (e.g., `Europe/Berlin`). |
| 22 | `price_range` | string | Price indicator (e.g. `$`, `$$`, `$$$`). |
| 23 | `data_id` | string | Internal Google Maps data ID. |
| 24 | `street_view_url` | string | URL pointing to Google Street View imagery. |
| 25 | `place_id` | string | **Google's globally unique Place ID** (best for deduplication). |
| 26 | `images` | string | Pipe- or comma-separated additional image URLs. |
| 27 | `reservations` | string | Reservation link (OpenTable, Resy, SevenRooms, etc.). |
| 28 | `order_online` | string | Online food/delivery order URL. |
| 29 | `menu` | string | Link to menu or services page. |
| 30 | `owner` | string | Ownership status (`Claimed` or `Unclaimed`). |
| 31 | `complete_address` | string | Fully formatted multi-part postal address. |
| 32 | `credit_cards_accepted` | string | Comma-separated list of accepted payment cards. |
| 33 | `about` | string (JSON) | Amenities, accessibility, and service options. |
| 34 | `user_reviews` | string (JSON) | Top review excerpts with author, rating, and date. |
| 35 | `user_reviews_extended` | string (JSON) | Extended review dataset (populated if `extra_reviews: true`). |
| 36 | `emails` | string | Comma-separated email addresses scraped from official website. |

---

## 7. Recommended CRM Field Mapping

When saving leads into your CRM database, map the CSV fields as follows:

```
Google Maps Scraper CSV            CRM Lead Database
-----------------------            -----------------
title                    ====>     company_name / lead_name
category                 ====>     industry / tags
phone                    ====>     phone_number
emails                   ====>     email_address
website                  ====>     website_url
complete_address         ====>     full_address
review_rating            ====>     google_rating
review_count             ====>     total_reviews
latitude / longitude     ====>     lat / lng
place_id                 ====>     source_unique_id (Deduplication Key)
link                     ====>     google_maps_url
```

> [!IMPORTANT]
> Always check for existing leads using `place_id` or `phone` before inserting, to avoid duplicates across multiple scraping campaigns.

---

## 8. End-to-End Implementation Examples

### PHP / Laravel Example

#### 1. Dispatching Job from CRM Controller
```php
<?php

namespace App\Http\Controllers;

use Illuminate\Http\Request;
use Illuminate\Support\Facades\Http;
use App\Models\ScrapeTask;

class ScraperController extends Controller
{
    public function startScrape(Request $request)
    {
        $scraperHost = config('services.scraper.url', 'http://localhost:8080');
        $scraperKey  = config('services.scraper.key', 'my-secure-api-key');

        $response = Http::withHeaders([
            'X-API-Key' => $scraperKey,
        ])->post("{$scraperHost}/api/v1/jobs", [
            'name'        => $request->input('job_name', 'Lead Generation'),
            'keywords'    => [$request->input('keyword', 'dentists in berlin')],
            'lang'        => 'en',
            'depth'       => (int) $request->input('depth', 1),
            'max_time'    => 300,
            'email'       => true,
            'webhook_url' => route('api.webhooks.google_maps'),
        ]);

        if ($response->successful()) {
            $jobId = $response->json('id');

            ScrapeTask::create([
                'job_id'  => $jobId,
                'keyword' => $request->input('keyword'),
                'status'  => 'processing',
            ]);

            return response()->json([
                'message' => 'Scraping job started successfully',
                'job_id'  => $jobId,
            ], 201);
        }

        return response()->json(['error' => 'Failed to start scraper'], 500);
    }
}
```

#### 2. Handling Webhook & Importing Leads
```php
<?php

namespace App\Http\Controllers;

use Illuminate\Http\Request;
use Illuminate\Support\Facades\Http;
use App\Models\Lead;
use App\Models\ScrapeTask;

class WebhookController extends Controller
{
    public function handleScraperWebhook(Request $request)
    {
        $event  = $request->input('event');
        $status = $request->input('status');
        $jobId  = $request->input('id');

        $task = ScrapeTask::where('job_id', $jobId)->first();
        if (!$task) {
            return response()->json(['message' => 'Job not found'], 404);
        }

        if ($event === 'job.completed' && $status === 'ok') {
            $scraperHost = config('services.scraper.url', 'http://localhost:8080');
            $downloadUrl = "{$scraperHost}/api/v1/jobs/{$jobId}/download";

            // Stream and parse CSV
            $fileStream = fopen($downloadUrl, 'r');
            if ($fileStream !== false) {
                $headers = fgetcsv($fileStream);

                while (($row = fgetcsv($fileStream)) !== false) {
                    $rowAssoc = array_combine($headers, $row);

                    $placeId = $rowAssoc['place_id'] ?? null;
                    if (!$placeId) continue;

                    // Deduplication check
                    Lead::updateOrCreate(
                        ['google_place_id' => $placeId],
                        [
                            'company_name' => $rowAssoc['title'] ?? '',
                            'phone'        => $rowAssoc['phone'] ?? '',
                            'email'        => $rowAssoc['emails'] ?? '',
                            'website'      => $rowAssoc['website'] ?? '',
                            'category'     => $rowAssoc['category'] ?? '',
                            'address'      => $rowAssoc['complete_address'] ?: ($rowAssoc['address'] ?? ''),
                            'rating'       => (float) ($rowAssoc['review_rating'] ?? 0),
                            'reviews'      => (int) ($rowAssoc['review_count'] ?? 0),
                            'maps_url'     => $rowAssoc['link'] ?? '',
                        ]
                    );
                }
                fclose($fileStream);
            }

            $task->update(['status' => 'completed']);
            return response()->json(['message' => 'Leads imported successfully'], 200);
        }

        $task->update([
            'status' => 'failed',
            'error'  => $request->input('error'),
        ]);

        return response()->json(['message' => 'Job failed handled'], 200);
    }
}
```

---

### Node.js / Express Example

```javascript
const express = require('express');
const axios = require('axios');
const csv = require('csv-parser');
const app = express();

app.use(express.json());

const SCRAPER_URL = process.env.SCRAPER_URL || 'http://localhost:8080';
const SCRAPER_KEY = process.env.SCRAPER_API_KEY || 'my-secure-api-key';

// 1. Dispatch Scraping Job
app.post('/api/start-scrape', async (req, res) => {
  try {
    const { keyword, depth = 1 } = req.body;

    const response = await axios.post(`${SCRAPER_URL}/api/v1/jobs`, {
      name: `Search - ${keyword}`,
      keywords: [keyword],
      lang: 'en',
      depth: depth,
      max_time: 300,
      email: true,
      webhook_url: 'https://my-crm.com/api/webhooks/google-maps'
    }, {
      headers: {
        'X-API-Key': SCRAPER_KEY
      }
    });

    res.status(201).json({ jobId: response.data.id });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

// 2. Webhook Endpoint
app.post('/api/webhooks/google-maps', async (req, res) => {
  const { event, status, id, download_url } = req.body;

  if (event === 'job.completed' && status === 'ok') {
    try {
      const csvResponse = await axios.get(`${SCRAPER_URL}${download_url}`, {
        headers: { 'X-API-Key': SCRAPER_KEY },
        responseType: 'stream'
      });

      csvResponse.data
        .pipe(csv())
        .on('data', async (row) => {
          // Process and save row into CRM database
          console.log(`Lead: ${row.title} | Phone: ${row.phone} | Email: ${row.emails}`);
        })
        .on('end', () => {
          console.log('All leads successfully imported.');
        });

      return res.status(200).json({ message: 'Webhook received' });
    } catch (err) {
      console.error('Error downloading CSV:', err);
      return res.status(500).json({ error: 'Failed to process CSV' });
    }
  }

  res.status(200).json({ message: 'Received non-completion event' });
});

app.listen(3000, () => console.log('CRM Server running on port 3000'));
```

