package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type alertRule struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Severity    string `json:"severity,omitempty"`
}

type alertContext struct {
	ID        string    `json:"id"`
	Value     any       `json:"value"`
	Timestamp string    `json:"timestamp"`
	Rule      alertRule `json:"rule"`
}

type analyzeRequest struct {
	Namespace   string       `json:"namespace"`
	Project     string       `json:"project"`
	Component   string       `json:"component"`
	Environment string       `json:"environment"`
	Alert       alertContext `json:"alert"`
}

type service struct {
	db     *sql.DB
	afmURL string
	client *http.Client
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	afmURL := envOrDefault("AFM_AGENT_URL", "http://localhost:8085")
	listenAddr := envOrDefault("LISTEN_ADDR", ":8090")
	dbPath := envOrDefault("DB_PATH", "data/reports.db")

	db, err := initDB(dbPath)
	if err != nil {
		slog.Error("failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	svc := &service{
		db:     db,
		afmURL: afmURL,
		client: &http.Client{Timeout: 30 * time.Minute},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1alpha1/rca-agent/analyze", svc.handleAnalyze)
	mux.HandleFunc("GET /api/v1/rca-agent/reports", svc.handleListReports)
	mux.HandleFunc("GET /api/v1/rca-agent/reports/{reportID}", svc.handleGetReport)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	srv := &http.Server{Addr: listenAddr, Handler: cors(mux)}

	go func() {
		slog.Info("agent-service starting", "addr", listenAddr, "afm_url", afmURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	slog.Info("agent-service stopped")
}

// --- handlers ---

func (s *service) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}

	var req analyzeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	reportID := fmt.Sprintf("%s_%d", req.Alert.ID, time.Now().Unix())

	_, err = s.db.Exec(
		`INSERT INTO rca_reports (report_id, alert_id, status, timestamp, environment, project, namespace)
		 VALUES (?, ?, 'pending', ?, ?, ?, ?)`,
		reportID, req.Alert.ID, time.Now().UTC().Format(time.RFC3339),
		req.Environment, req.Project, req.Namespace,
	)
	if err != nil {
		slog.Error("failed to create report", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create report"})
		return
	}

	go s.runAnalysis(reportID, body)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"reportId": reportID,
		"status":   "pending",
	})
}

func (s *service) handleListReports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project := q.Get("project")
	environment := q.Get("environment")
	namespace := q.Get("namespace")
	startTime := q.Get("startTime")
	endTime := q.Get("endTime")
	status := q.Get("status")
	sortDir := q.Get("sort")

	if project == "" || environment == "" || namespace == "" || startTime == "" || endTime == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "project, environment, namespace, startTime, endTime are required",
		})
		return
	}

	order := "DESC"
	if sortDir == "asc" {
		order = "ASC"
	}

	query := `SELECT report_id, alert_id, timestamp, summary, status FROM rca_reports
		WHERE project = ? AND environment = ? AND namespace = ?
		AND timestamp >= ? AND timestamp <= ?`
	args := []any{project, environment, namespace, startTime, endTime}

	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	query += fmt.Sprintf(" ORDER BY timestamp %s LIMIT 100", order)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		slog.Error("query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	reports := []map[string]any{}
	for rows.Next() {
		var reportID, alertID, ts, sts string
		var summary sql.NullString
		if err := rows.Scan(&reportID, &alertID, &ts, &summary, &sts); err != nil {
			continue
		}
		entry := map[string]any{
			"reportId":  reportID,
			"alertId":   alertID,
			"timestamp": ts,
			"status":    sts,
		}
		if summary.Valid {
			entry["summary"] = summary.String
		}
		reports = append(reports, entry)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"reports":    reports,
		"totalCount": len(reports),
	})
}

func (s *service) handleGetReport(w http.ResponseWriter, r *http.Request) {
	reportID := r.PathValue("reportID")

	var alertID, ts, status string
	var summary, reportData sql.NullString

	err := s.db.QueryRow(
		`SELECT alert_id, timestamp, status, summary, report FROM rca_reports WHERE report_id = ?`,
		reportID,
	).Scan(&alertID, &ts, &status, &summary, &reportData)

	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "report not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}

	result := map[string]any{
		"reportId":  reportID,
		"alertId":   alertID,
		"timestamp": ts,
		"status":    status,
	}
	if summary.Valid {
		result["summary"] = summary.String
	}
	if reportData.Valid {
		var report map[string]any
		if err := json.Unmarshal([]byte(reportData.String), &report); err == nil {
			result["report"] = report
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// --- background ---

func (s *service) runAnalysis(reportID string, payload []byte) {
	slog.Info("starting analysis", "report_id", reportID)

	webhookURL := s.afmURL + "/api/v1alpha1/rca-agent/analyze"
	resp, err := s.client.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		slog.Error("afm agent call failed", "report_id", reportID, "error", err)
		s.setStatus(reportID, "failed")
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("failed to read afm response", "report_id", reportID, "error", err)
		s.setStatus(reportID, "failed")
		return
	}

	if resp.StatusCode != http.StatusOK {
		slog.Error("afm agent returned error", "report_id", reportID, "status", resp.StatusCode, "body", string(respBody))
		s.setStatus(reportID, "failed")
		return
	}

	var report map[string]any
	if err := json.Unmarshal(respBody, &report); err != nil {
		slog.Error("failed to parse afm response", "report_id", reportID, "error", err)
		s.setStatus(reportID, "failed")
		return
	}

	summary, _ := report["summary"].(string)
	reportJSON, _ := json.Marshal(report)

	_, err = s.db.Exec(
		`UPDATE rca_reports SET status = 'completed', summary = ?, report = ? WHERE report_id = ?`,
		summary, string(reportJSON), reportID,
	)
	if err != nil {
		slog.Error("failed to update report", "report_id", reportID, "error", err)
		return
	}

	slog.Info("analysis completed", "report_id", reportID)
}

func (s *service) setStatus(reportID, status string) {
	if _, err := s.db.Exec(`UPDATE rca_reports SET status = ? WHERE report_id = ?`, status, reportID); err != nil {
		slog.Error("failed to update report status", "report_id", reportID, "error", err)
	}
}

// --- helpers ---

func initDB(path string) (*sql.DB, error) {
	os.MkdirAll(filepath.Dir(path), 0o755)

	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS rca_reports (
			report_id   TEXT PRIMARY KEY,
			alert_id    TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'pending',
			summary     TEXT,
			timestamp   TEXT NOT NULL,
			environment TEXT,
			project     TEXT,
			namespace   TEXT,
			report      TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_alert_id ON rca_reports(alert_id);
		CREATE INDEX IF NOT EXISTS idx_project_env ON rca_reports(project, environment);
		CREATE INDEX IF NOT EXISTS idx_timestamp ON rca_reports(timestamp);
		CREATE INDEX IF NOT EXISTS idx_status ON rca_reports(status);
	`)
	if err != nil {
		return nil, err
	}

	slog.Info("database initialized", "path", path)
	return db, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
