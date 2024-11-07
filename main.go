package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	_ "embed"

	_ "github.com/lib/pq"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/js"
	"github.com/ua-parser/uap-go/uaparser"
)

var (
	hostDomain  = os.Getenv("DOMAIN")
	apiKey      = os.Getenv("API_KEY")
	environment = os.Getenv("ENVIRONMENT")
	logLevel    = os.Getenv("LOG_LEVEL")
)

//go:embed tracking.js
var trackingJS string

//go:embed regexp.yaml
var userAgentRegexp string

//go:embed index.html
var indexHTML string

func main() {
	err := loadEnv(".env")
	if err != nil {
		log.Fatalf("Failed to load environment variables: %v", err)
	}

	// Set up structured logging using slog
	var level slog.Level
	if environment == "production" {
		level = slog.LevelInfo
	} else {
		level = slog.LevelDebug
	}

	// If LOG_LEVEL is set, use it to override the default level
	if logLevel != "" {
		switch logLevel {
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}

	logger := slog.New(slog.NewTextHandler(log.Writer(), &slog.HandlerOptions{Level: level}))

	// Establish a connection to the PostgreSQL database
	db, err := sql.Open("postgres", getConnStr())
	if err != nil {
		log.Fatalf("Failed to connect to the database: %v", err)
	}
	defer db.Close()

	// Ensure the database connection is working
	err = db.Ping()
	if err != nil {
		log.Fatalf("Failed to ping the database: %v", err)
	}

	// Create the HLL extension if it doesn't exist
	_, err = db.Exec(`CREATE EXTENSION IF NOT EXISTS hll`)
	if err != nil {
		log.Fatalf("Failed to create hll extension: %v", err)
	}

	// Ensure the pageviews table exists
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS pageviews (
		domain TEXT NOT NULL,
		path TEXT NOT NULL,
		day DATE NOT NULL,
		visitor_hll hll NOT NULL,
		UNIQUE (domain, day, path)
	);
	CREATE INDEX IF NOT EXISTS pageviews_day_idx ON pageviews (day DESC);`)
	if err != nil {
		log.Fatalf("Failed to create pageviews table: %v", err)
	}

	// Load the User-Agent parser
	parser, err := uaparser.NewFromBytes([]byte(userAgentRegexp))
	if err != nil {
		log.Fatalf("Failed to load User-Agent parser: %v", err)
	}

	http.HandleFunc("/track", func(w http.ResponseWriter, r *http.Request) {
		visitedURL := r.FormValue("url")
		if visitedURL == "" {
			logger.Warn("Missing 'url' parameter in request", slog.String("remote_addr", r.RemoteAddr))
			http.Error(w, "Missing 'url' parameter", http.StatusBadRequest)
			return
		}

		ua := r.Header.Get("User-Agent")
		client := parser.Parse(ua)
		if client.Device.Family == "Spider" || client.UserAgent.Family == "Bot" {
			logger.Debug("Ignored non-human pageview", slog.String("url", visitedURL), slog.String("user_agent", ua), slog.String("remote_addr", r.RemoteAddr))
			w.WriteHeader(http.StatusOK)
			return
		}

		day := time.Now().UTC().Truncate(24 * time.Hour)

		visitorIP := r.Header.Get("CF-Connecting-IP")
		if visitorIP == "" {
			visitorIP = r.RemoteAddr
		}

		// Parse the URL to extract domain and path
		parsedURL, err := url.Parse(visitedURL)
		if err != nil {
			logger.Error("Failed to parse URL", slog.String("url", visitedURL), slog.String("error", err.Error()))
			http.Error(w, "Invalid URL", http.StatusBadRequest)
			return
		}

		path := parsedURL.Path
		if path == "" {
			path = "/"
		}

		err = trackPageView(db, parsedURL.Host, path, day, visitorIP)
		if err != nil {
			logger.Error("Failed to track pageview", slog.String("url", visitedURL), slog.String("visitor_ip", visitorIP), slog.String("error", err.Error()))
			http.Error(w, fmt.Sprintf("Failed to track pageview: %v", err), http.StatusInternalServerError)
			return
		}

		logger.Debug("Pageview tracked", slog.String("url", visitedURL), slog.String("visitor_ip", visitorIP), slog.String("user_agent", ua))

		if r.URL.Query().Get("url") != "" {
			w.Header().Set("Cache-Control", "public, max-age=3600, s-maxage=3600, must-revalidate")
		}
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if apiKey != "" && r.URL.Query().Get("api_key") != apiKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		domain := r.URL.Query().Get("domain")
		if domain == "" {
			http.Error(w, "Missing domain parameter", http.StatusBadRequest)
			return
		}

		// Get stats for the last 30 days by default
		endTime := time.Now().UTC().Truncate(24 * time.Hour)
		startTime := endTime.Add(-30 * 24 * time.Hour)

		// Check if domain-level stats are requested
		aggregate := r.URL.Query().Get("aggregate") == "true"

		type PageStat struct {
			Path     string    `json:"path,omitempty"`
			Day      time.Time `json:"day"`
			Visitors int       `json:"visitors"`
		}

		var query string
		if aggregate {
			query = `
			SELECT day, #(hll_union_agg(visitor_hll)) as visitors
			FROM pageviews
			WHERE domain = $1 AND day >= $2 AND day <= $3
			GROUP BY day
				ORDER BY day DESC
			`
		} else {
			query = `
			SELECT path, day, hll_cardinality(visitor_hll) as visitors
			FROM pageviews
			WHERE domain = $1 AND day >= $2 AND day <= $3
			ORDER BY day DESC, visitors DESC
			`
		}

		rows, err := db.Query(query, domain, startTime, endTime)
		if err != nil {
			logger.Error("Failed to query stats", slog.String("error", err.Error()))
			http.Error(w, "Failed to fetch stats", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var stats []PageStat
		for rows.Next() {
			var stat PageStat
			var err error
			if aggregate {
				err = rows.Scan(&stat.Day, &stat.Visitors)
			} else {
				err = rows.Scan(&stat.Path, &stat.Day, &stat.Visitors)
			}
			if err != nil {
				logger.Error("Failed to scan stats", slog.String("error", err.Error()))
				http.Error(w, "Failed to fetch stats", http.StatusInternalServerError)
				return
			}
			stats = append(stats, stat)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(stats)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, indexHTML)
	})

	http.HandleFunc("/analytics.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "public, max-age=86400") // Cache for 24 hours

		url := "https://" + hostDomain + "/track"

		if hostDomain == "localhost" {
			url = "http://localhost:8080/track"
		}

		script, err := jsMinifier.String("text/javascript", fmt.Sprintf(trackingJS, url))
		if err != nil {
			logger.Error("Failed to minify tracking.js", slog.String("error", err.Error()))
			http.Error(w, "Failed to minify tracking.js", http.StatusInternalServerError)
			return
		}

		w.Write([]byte(script))
	})

	logger.Info("Starting server", slog.String("address", ":8080"))
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func trackPageView(db *sql.DB, domain string, path string, day time.Time, visitor string) error {
	hash := fmt.Sprintf("%x", visitor)

	query := `
	INSERT INTO pageviews (domain, path, day, visitor_hll)
	VALUES ($1, $2, $3, hll_add(hll_empty(), hll_hash_text($4)))
	ON CONFLICT (domain, day, path)
	DO UPDATE SET visitor_hll = hll_add(pageviews.visitor_hll, hll_hash_text($4))
	`

	_, err := db.Exec(query, domain, path, day, hash)
	if err != nil {
		return fmt.Errorf("failed to execute query: %w", err)
	}

	return nil
}

func getConnStr() string {
	if value := os.Getenv("DATABASE_URL"); value != "" {
		return value
	}
	return "postgres://postgres@localhost:5432/potato?sslmode=disable"
}

func loadEnv(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		// If the file doesn't exist, return nil
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split on first '=' only
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		// Trim spaces and quotes
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, `"'`)

		// Set environment variable
		os.Setenv(key, value)
	}

	return scanner.Err()
}

var jsMinifier *minify.M

func init() {
	jsMinifier = minify.New()
	jsMinifier.AddFunc("text/javascript", js.Minify)
}
