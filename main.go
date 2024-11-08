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
	hostDomain  string
	apiKey      string
	environment string
	logLevel    string
)

//go:embed tracking.js
var trackingJS string

//go:embed regexp.yaml
var userAgentRegexp string

//go:embed index.html
var indexHTML string

func main() {
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

	// Merge all table creation statements
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS pages (
			domain TEXT NOT NULL,
			path TEXT NOT NULL,
			day DATE NOT NULL,
			visitor_hll hll NOT NULL,
			UNIQUE (domain, day, path)
		);
		CREATE INDEX IF NOT EXISTS pages_day_idx ON pages (day DESC);

		CREATE TABLE IF NOT EXISTS countries (
			domain TEXT NOT NULL,
			country TEXT NOT NULL,
			day DATE NOT NULL,
			visitor_hll hll NOT NULL,
			UNIQUE (domain, day, country)
		);
		CREATE INDEX IF NOT EXISTS countries_day_idx ON countries (day DESC);

		CREATE TABLE IF NOT EXISTS sources (
			domain TEXT NOT NULL,
			referrer TEXT NOT NULL,
			day DATE NOT NULL,
			visitor_hll hll NOT NULL,
			UNIQUE (domain, day, referrer)
		);
		CREATE INDEX IF NOT EXISTS sources_day_idx ON sources (day DESC);`)
	if err != nil {
		log.Fatalf("Failed to create tables: %v", err)
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

		country := r.Header.Get("CF-IPCountry")
		if country != "" {
			err = trackCountryView(db, parsedURL.Host, country, day, visitorIP)
			if err != nil {
				logger.Error("Failed to track country view", slog.String("error", err.Error()))
			}
		}

		referrer := r.Header.Get("Referer")
		if referrer == "" {
			referrer = "Direct / None"
		} else {
			// Parse referrer to get domain only
			if refURL, err := url.Parse(referrer); err == nil {
				referrer = refURL.Host
			}
		}

		// If the referrer is the same as the domain, set it to "Direct / None"
		if referrer == parsedURL.Host {
			referrer = "Direct / None"
		}

		err = trackSourceView(db, parsedURL.Host, referrer, day, visitorIP)
		if err != nil {
			logger.Error("Failed to track source view", slog.String("error", err.Error()))
		}

		logger.Debug("Pageview tracked", slog.String("url", visitedURL), slog.String("visitor_ip", visitorIP), slog.String("user_agent", ua))

		if r.URL.Query().Get("url") != "" {
			w.Header().Set("Cache-Control", "public, max-age=3600, s-maxage=3600, must-revalidate")
		}
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/stats/pages", requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
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
			FROM pages
			WHERE domain = $1 AND day >= $2 AND day <= $3
			GROUP BY day
				ORDER BY day DESC
			`
		} else {
			query = `
			SELECT path, day, hll_cardinality(visitor_hll) as visitors
			FROM pages
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
	}))

	http.HandleFunc("/stats/sources", requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		domain := r.URL.Query().Get("domain")
		if domain == "" {
			http.Error(w, "Missing domain parameter", http.StatusBadRequest)
			return
		}

		endTime := time.Now().UTC().Truncate(24 * time.Hour)
		startTime := endTime.Add(-30 * 24 * time.Hour)

		type SourceStat struct {
			Referrer string    `json:"referrer"`
			Day      time.Time `json:"day"`
			Visitors int       `json:"visitors"`
		}

		query := `
		SELECT referrer, day, hll_cardinality(visitor_hll) as visitors
		FROM sources
		WHERE domain = $1 AND day >= $2 AND day <= $3
		ORDER BY day DESC, visitors DESC
		`

		rows, err := db.Query(query, domain, startTime, endTime)
		if err != nil {
			logger.Error("Failed to query stats", slog.String("error", err.Error()))
			http.Error(w, "Failed to fetch stats", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var stats []SourceStat
		for rows.Next() {
			var stat SourceStat
			if err := rows.Scan(&stat.Referrer, &stat.Day, &stat.Visitors); err != nil {
				logger.Error("Failed to scan stats", slog.String("error", err.Error()))
				http.Error(w, "Failed to fetch stats", http.StatusInternalServerError)
				return
			}
			stats = append(stats, stat)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(stats)
	}))

	http.HandleFunc("/stats/countries", requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		domain := r.URL.Query().Get("domain")
		if domain == "" {
			http.Error(w, "Missing domain parameter", http.StatusBadRequest)
			return
		}

		endTime := time.Now().UTC().Truncate(24 * time.Hour)
		startTime := endTime.Add(-30 * 24 * time.Hour)

		type CountryStat struct {
			Country  string    `json:"country"`
			Day      time.Time `json:"day"`
			Visitors int       `json:"visitors"`
		}

		query := `
		SELECT country, day, hll_cardinality(visitor_hll) as visitors
		FROM countries
		WHERE domain = $1 AND day >= $2 AND day <= $3
		ORDER BY day DESC, visitors DESC
		`

		rows, err := db.Query(query, domain, startTime, endTime)
		if err != nil {
			logger.Error("Failed to query stats", slog.String("error", err.Error()))
			http.Error(w, "Failed to fetch stats", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var stats []CountryStat
		for rows.Next() {
			var stat CountryStat
			if err := rows.Scan(&stat.Country, &stat.Day, &stat.Visitors); err != nil {
				logger.Error("Failed to scan stats", slog.String("error", err.Error()))
				http.Error(w, "Failed to fetch stats", http.StatusInternalServerError)
				return
			}
			stats = append(stats, stat)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(stats)
	}))

	http.HandleFunc("/analytics.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "public, max-age=86400") // Cache for 24 hours

		var url string
		switch hostDomain {
		case "":
			logger.Error("HOST_DOMAIN is not set")
			http.Error(w, "HOST_DOMAIN is not set", http.StatusInternalServerError)
			return
		case "localhost":
			url = "http://localhost:8080/track"
		default:
			url = "https://" + hostDomain + "/track"
		}

		script, err := jsMinifier.String("text/javascript", fmt.Sprintf(trackingJS, url))
		if err != nil {
			logger.Error("Failed to minify tracking.js", slog.String("error", err.Error()))
			http.Error(w, "Failed to minify tracking.js", http.StatusInternalServerError)
			return
		}

		w.Write([]byte(script))
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, indexHTML)
	})

	logger.Info("Starting server", slog.String("address", ":8080"))
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func trackPageView(db *sql.DB, domain string, path string, day time.Time, visitor string) error {
	hash := fmt.Sprintf("%x", visitor)

	query := `
	INSERT INTO pages (domain, path, day, visitor_hll)
	VALUES ($1, $2, $3, hll_add(hll_empty(), hll_hash_text($4)))
	ON CONFLICT (domain, day, path)
	DO UPDATE SET visitor_hll = hll_add(pages.visitor_hll, hll_hash_text($4))
	`

	_, err := db.Exec(query, domain, path, day, hash)
	if err != nil {
		return fmt.Errorf("failed to execute query: %w", err)
	}

	return nil
}

func trackCountryView(db *sql.DB, domain string, country string, day time.Time, visitor string) error {
	hash := fmt.Sprintf("%x", visitor)

	query := `
	INSERT INTO countries (domain, country, day, visitor_hll)
	VALUES ($1, $2, $3, hll_add(hll_empty(), hll_hash_text($4)))
	ON CONFLICT (domain, day, country)
	DO UPDATE SET visitor_hll = hll_add(countries.visitor_hll, hll_hash_text($4))
	`

	_, err := db.Exec(query, domain, country, day, hash)
	if err != nil {
		return fmt.Errorf("failed to track country view: %w", err)
	}

	return nil
}

func trackSourceView(db *sql.DB, domain string, referrer string, day time.Time, visitor string) error {
	hash := fmt.Sprintf("%x", visitor)

	query := `
	INSERT INTO sources (domain, referrer, day, visitor_hll)
	VALUES ($1, $2, $3, hll_add(hll_empty(), hll_hash_text($4)))
	ON CONFLICT (domain, day, referrer)
	DO UPDATE SET visitor_hll = hll_add(sources.visitor_hll, hll_hash_text($4))
	`

	_, err := db.Exec(query, domain, referrer, day, hash)
	if err != nil {
		return fmt.Errorf("failed to track source view: %w", err)
	}

	return nil
}

func getConnStr() string {
	if value := os.Getenv("DATABASE_URL"); value != "" {
		return value
	}
	return "postgres://postgres@localhost:5432/potato?sslmode=disable"
}

// init loads environment variables from .env file
func init() {
	file, err := os.Open(".env")
	if err != nil {
		// If the file doesn't exist, return nil
		if errors.Is(err, os.ErrNotExist) {
			return
		}

		panic(err)
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

	if err := scanner.Err(); err != nil {
		panic(err)
	}

	hostDomain = os.Getenv("HOST_DOMAIN")
	apiKey = os.Getenv("API_KEY")
	environment = os.Getenv("ENVIRONMENT")
	logLevel = os.Getenv("LOG_LEVEL")
}

var jsMinifier *minify.M

func init() {
	jsMinifier = minify.New()
	jsMinifier.AddFunc("text/javascript", js.Minify)
}

func requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" && environment == "production" {
			http.Error(w, "API_KEY is mandatory in production", http.StatusUnauthorized)
			return
		}

		if apiKey != "" && r.URL.Query().Get("api_key") != apiKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
