package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"github.com/robfig/cron/v3"

	"ansible-api/config"
)

type PlaybookRequest struct {
	Playbook  string            `json:"playbook"`
	Inventory string            `json:"inventory,omitempty"`
	ExtraVars map[string]string `json:"extra_vars,omitempty"`
}

type PlaybookLog struct {
	ID        int       `json:"id"`
	Playbook  string    `json:"playbook"`
	Success   bool      `json:"success"`
	Output    string    `json:"output"`
	Error     string    `json:"error"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Duration  float64   `json:"duration"`
}

type PlaybookRunStatus string

const (
	RunStatusStarted   PlaybookRunStatus = "started"
	RunStatusCompleted PlaybookRunStatus = "completed"
	RunStatusFailed    PlaybookRunStatus = "failed"
)

type PlaybookRun struct {
	ID          int               `json:"id"`
	Playbook    string            `json:"playbook"`
	Inventory   string            `json:"inventory,omitempty"`
	Status      PlaybookRunStatus `json:"status"`
	StartTime   time.Time         `json:"start_time"`
	EndTime     *time.Time        `json:"end_time,omitempty"`
	Duration    *float64          `json:"duration,omitempty"`
	TriggeredBy string            `json:"triggered_by,omitempty"`
	ExtraVars   map[string]string `json:"extra_vars,omitempty"`
	Output      string            `json:"output,omitempty"`
	Error       string            `json:"error,omitempty"`
}

type LogsResponse struct {
	Logs        []PlaybookLog `json:"logs"`
	TotalCount  int           `json:"total_count"`
	CurrentPage int           `json:"current_page"`
	TotalPages  int           `json:"total_pages"`
}

type RunsResponse struct {
	Runs        []PlaybookRun `json:"runs"`
	TotalCount  int           `json:"total_count"`
	CurrentPage int           `json:"current_page"`
	TotalPages  int           `json:"total_pages"`
}

var (
	cfg     *config.Config
	mutex   = &sync.Mutex{}
	db      *sql.DB
	cronSvc *cron.Cron
)

func init() {
	var err error
	cfg, err = config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	cronSvc = cron.New()
	_, err = cronSvc.AddFunc("@daily", cleanupOldLogs)
	if err != nil {
		log.Fatalf("Failed to schedule log cleanup: %v", err)
	}
	cronSvc.Start()
}

func main() {
	if err := initDB(); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()

	if err := applyMigrations(); err != nil {
		log.Fatalf("Failed to apply migrations: %v", err)
	}

	server := &http.Server{
		Addr:         ":" + cfg.Server.Port,
		Handler:      nil,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	http.HandleFunc("/api/run", runPlaybookHandler)
	http.HandleFunc("/api/playbooks", listPlaybooksHandler)
	http.HandleFunc("/api/logs", listLogsHandler)
	http.HandleFunc("/api/logs/", getLogHandler)
	http.HandleFunc("/api/runs", getPlaybookRunsHandler)
	http.HandleFunc("/api/runs/", getPlaybookRunDetailsHandler)

	log.Printf("Server started on :%s", cfg.Server.Port)
	log.Fatal(server.ListenAndServe())
}

func initDB() error {
	psqlInfo := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Database.Host,
		cfg.Database.Port,
		cfg.Database.User,
		cfg.Database.Password,
		cfg.Database.Name,
		cfg.Database.SSLMode,
	)

	var err error
	db, err = sql.Open("postgres", psqlInfo)
	if err != nil {
		return err
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	return db.Ping()
}

func applyMigrations() error {
	var tableExists bool
	err := db.QueryRow("SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'migrations')").Scan(&tableExists)
	if err != nil {
		return err
	}

	if !tableExists {
		migrationSQL, err := os.ReadFile("migrations/001_initial.sql")
		if err != nil {
			return err
		}

		if _, err := db.Exec(string(migrationSQL)); err != nil {
			return err
		}

		if _, err := db.Exec("INSERT INTO migrations (name) VALUES ($1)", "001_initial"); err != nil {
			return err
		}
	}

	return nil
}

func cleanupOldLogs() {
	log.Printf("Starting cleanup of logs older than %d days", cfg.Logging.RetentionDays)

	retentionPeriod := time.Now().AddDate(0, 0, -cfg.Logging.RetentionDays)

	result, err := db.Exec(`
		DELETE FROM playbook_logs 
		WHERE start_time < $1;
		
		DELETE FROM playbook_runs
		WHERE start_time < $1;
	`, retentionPeriod)

	if err != nil {
		log.Printf("Error cleaning up old logs: %v", err)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	log.Printf("Cleaned up %d old log entries", rowsAffected)
}

func runPlaybookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req PlaybookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	playbookPath := filepath.Join(cfg.Server.PlaybooksDir, req.Playbook)
	if _, err := os.Stat(playbookPath); os.IsNotExist(err) {
		http.Error(w, "Playbook not found", http.StatusNotFound)
		return
	}

	remoteAddr := r.RemoteAddr
	if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
		remoteAddr = forwardedFor
	}

	runID, err := logPlaybookStart(req, remoteAddr)
	if err != nil {
		log.Printf("Failed to log playbook start: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	go func() {
		mutex.Lock()
		defer mutex.Unlock()

		startTime := time.Now()
		output, err := runAnsiblePlaybook(playbookPath, req.Inventory, req.ExtraVars)
		endTime := time.Now()
		duration := endTime.Sub(startTime).Seconds()

		// Log to playbook_logs
		success := err == nil
		errorMsg := ""
		if err != nil {
			errorMsg = err.Error()
		}
		_, _ = logExecution(req.Playbook, success, output, errorMsg, startTime, endTime, duration)

		// Update playbook_runs
		if err != nil {
			_ = updatePlaybookRun(runID, RunStatusFailed, output, err.Error())
		} else {
			_ = updatePlaybookRun(runID, RunStatusCompleted, output, "")
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "accepted",
		"message": "playbook execution started",
		"run_id":  runID,
	})
}

func listPlaybooksHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	files, err := os.ReadDir(cfg.Server.PlaybooksDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var playbooks []string
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".yml" {
			playbooks = append(playbooks, file.Name())
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(playbooks)
}

func listLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	queryParams := r.URL.Query()
	page, _ := strconv.Atoi(queryParams.Get("page"))
	if page < 1 {
		page = 1
	}

	successFilter := queryParams.Get("success")
	playbookFilter := queryParams.Get("playbook")
	dateFrom := queryParams.Get("from")
	dateTo := queryParams.Get("to")

	baseQuery := "SELECT id, playbook, success, output, error, start_time, end_time, duration FROM playbook_logs"
	countQuery := "SELECT COUNT(*) FROM playbook_logs"

	var filters []string
	var args []interface{}
	argPos := 1

	if successFilter != "" {
		success, err := strconv.ParseBool(successFilter)
		if err == nil {
			filters = append(filters, fmt.Sprintf("success = $%d", argPos))
			args = append(args, success)
			argPos++
		}
	}

	if playbookFilter != "" {
		filters = append(filters, fmt.Sprintf("playbook = $%d", argPos))
		args = append(args, playbookFilter)
		argPos++
	}

	if dateFrom != "" {
		if fromTime, err := time.Parse(time.RFC3339, dateFrom); err == nil {
			filters = append(filters, fmt.Sprintf("start_time >= $%d", argPos))
			args = append(args, fromTime)
			argPos++
		}
	}

	if dateTo != "" {
		if toTime, err := time.Parse(time.RFC3339, dateTo); err == nil {
			filters = append(filters, fmt.Sprintf("start_time <= $%d", argPos))
			args = append(args, toTime)
			argPos++
		}
	}

	whereClause := ""
	if len(filters) > 0 {
		whereClause = " WHERE " + strings.Join(filters, " AND ")
	}

	var totalCount int
	err := db.QueryRow(countQuery+whereClause, args...).Scan(&totalCount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	totalPages := (totalCount + cfg.Logging.PageSize - 1) / cfg.Logging.PageSize
	if page > totalPages && totalPages > 0 {
		page = totalPages
	}
	offset := (page - 1) * cfg.Logging.PageSize

	query := fmt.Sprintf("%s%s ORDER BY start_time DESC LIMIT %d OFFSET %d",
		baseQuery, whereClause, cfg.Logging.PageSize, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var logs []PlaybookLog
	for rows.Next() {
		var logEntry PlaybookLog
		err := rows.Scan(
			&logEntry.ID,
			&logEntry.Playbook,
			&logEntry.Success,
			&logEntry.Output,
			&logEntry.Error,
			&logEntry.StartTime,
			&logEntry.EndTime,
			&logEntry.Duration,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logs = append(logs, logEntry)
	}

	response := LogsResponse{
		Logs:        logs,
		TotalCount:  totalCount,
		CurrentPage: page,
		TotalPages:  totalPages,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func getLogHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := r.URL.Path[len("/api/logs/"):]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid log ID", http.StatusBadRequest)
		return
	}

	var logEntry PlaybookLog
	query := `SELECT id, playbook, success, output, error, start_time, end_time, duration 
	          FROM playbook_logs WHERE id = $1`

	err = db.QueryRow(query, id).Scan(
		&logEntry.ID,
		&logEntry.Playbook,
		&logEntry.Success,
		&logEntry.Output,
		&logEntry.Error,
		&logEntry.StartTime,
		&logEntry.EndTime,
		&logEntry.Duration,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Log not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(logEntry)
}

func getPlaybookRunsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	queryParams := r.URL.Query()
	page, _ := strconv.Atoi(queryParams.Get("page"))
	if page < 1 {
		page = 1
	}

	statusFilter := queryParams.Get("status")
	playbookFilter := queryParams.Get("playbook")
	dateFrom := queryParams.Get("from")
	dateTo := queryParams.Get("to")

	baseQuery := "SELECT id, playbook, inventory, status, start_time, end_time, duration, triggered_by FROM playbook_runs"
	countQuery := "SELECT COUNT(*) FROM playbook_runs"

	var filters []string
	var args []interface{}
	argPos := 1

	if statusFilter != "" {
		filters = append(filters, fmt.Sprintf("status = $%d", argPos))
		args = append(args, statusFilter)
		argPos++
	}

	if playbookFilter != "" {
		filters = append(filters, fmt.Sprintf("playbook = $%d", argPos))
		args = append(args, playbookFilter)
		argPos++
	}

	if dateFrom != "" {
		if fromTime, err := time.Parse(time.RFC3339, dateFrom); err == nil {
			filters = append(filters, fmt.Sprintf("start_time >= $%d", argPos))
			args = append(args, fromTime)
			argPos++
		}
	}

	if dateTo != "" {
		if toTime, err := time.Parse(time.RFC3339, dateTo); err == nil {
			filters = append(filters, fmt.Sprintf("start_time <= $%d", argPos))
			args = append(args, toTime)
			argPos++
		}
	}

	whereClause := ""
	if len(filters) > 0 {
		whereClause = " WHERE " + strings.Join(filters, " AND ")
	}

	var totalCount int
	err := db.QueryRow(countQuery+whereClause, args...).Scan(&totalCount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	totalPages := (totalCount + cfg.Logging.PageSize - 1) / cfg.Logging.PageSize
	if page > totalPages && totalPages > 0 {
		page = totalPages
	}
	offset := (page - 1) * cfg.Logging.PageSize

	query := fmt.Sprintf("%s%s ORDER BY start_time DESC LIMIT %d OFFSET %d",
		baseQuery, whereClause, cfg.Logging.PageSize, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var runs []PlaybookRun
	for rows.Next() {
		var run PlaybookRun
		var endTime sql.NullTime
		var duration sql.NullFloat64
		var inventory sql.NullString

		err := rows.Scan(
			&run.ID,
			&run.Playbook,
			&inventory,
			&run.Status,
			&run.StartTime,
			&endTime,
			&duration,
			&run.TriggeredBy,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if inventory.Valid {
			run.Inventory = inventory.String
		}
		if endTime.Valid {
			run.EndTime = &endTime.Time
		}
		if duration.Valid {
			run.Duration = &duration.Float64
		}

		runs = append(runs, run)
	}

	response := RunsResponse{
		Runs:        runs,
		TotalCount:  totalCount,
		CurrentPage: page,
		TotalPages:  totalPages,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func getPlaybookRunDetailsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := r.URL.Path[len("/api/runs/"):]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid run ID", http.StatusBadRequest)
		return
	}

	var run PlaybookRun
	var endTime sql.NullTime
	var duration sql.NullFloat64
	var inventory sql.NullString
	var extraVarsJSON []byte

	err = db.QueryRow(`
		SELECT 
			id, playbook, inventory, status, start_time, end_time, duration, 
			triggered_by, extra_vars, output, error
		FROM playbook_runs 
		WHERE id = $1
	`, id).Scan(
		&run.ID,
		&run.Playbook,
		&inventory,
		&run.Status,
		&run.StartTime,
		&endTime,
		&duration,
		&run.TriggeredBy,
		&extraVarsJSON,
		&run.Output,
		&run.Error,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Run not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if inventory.Valid {
		run.Inventory = inventory.String
	}
	if endTime.Valid {
		run.EndTime = &endTime.Time
	}
	if duration.Valid {
		run.Duration = &duration.Float64
	}

	if len(extraVarsJSON) > 0 {
		if err := json.Unmarshal(extraVarsJSON, &run.ExtraVars); err != nil {
			log.Printf("Failed to unmarshal extra_vars for run %d: %v", id, err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run)
}

func logPlaybookStart(req PlaybookRequest, remoteAddr string) (int, error) {
	extraVarsJSON, err := json.Marshal(req.ExtraVars)
	if err != nil {
		return 0, err
	}

	var runID int
	err = db.QueryRow(`
		INSERT INTO playbook_runs 
		(playbook, inventory, status, start_time, triggered_by, extra_vars)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`,
		req.Playbook,
		req.Inventory,
		RunStatusStarted,
		time.Now(),
		remoteAddr,
		extraVarsJSON,
	).Scan(&runID)

	return runID, err
}

func updatePlaybookRun(runID int, status PlaybookRunStatus, output, errorMsg string) error {
	var endTime time.Time
	var duration float64

	if status != RunStatusStarted {
		endTime = time.Now()
		var startTime time.Time
		err := db.QueryRow("SELECT start_time FROM playbook_runs WHERE id = $1", runID).Scan(&startTime)
		if err != nil {
			return err
		}
		duration = endTime.Sub(startTime).Seconds()
	}

	_, err := db.Exec(`
		UPDATE playbook_runs 
		SET 
			status = $1,
			end_time = $2,
			duration = $3,
			output = $4,
			error = $5
		WHERE id = $6
	`,
		status,
		endTime,
		duration,
		output,
		errorMsg,
		runID,
	)

	return err
}

func runAnsiblePlaybook(playbookPath, inventory string, extraVars map[string]string) (string, error) {
	args := []string{"ansible-playbook", playbookPath}

	if inventory != "" {
		inventoryPath := filepath.Join(cfg.Server.PlaybooksDir, inventory)
		if _, err := os.Stat(inventoryPath); err == nil {
			args = append(args, "-i", inventoryPath)
		}
	}

	if len(extraVars) > 0 {
		extraVarsStr := ""
		for k, v := range extraVars {
			if extraVarsStr != "" {
				extraVarsStr += " "
			}
			extraVarsStr += fmt.Sprintf("%s=%s", k, v)
		}
		args = append(args, "--extra-vars", extraVarsStr)
	}

	cmd := exec.Command(args[0], args[1:]...)
	output, err := cmd.CombinedOutput()

	return string(output), err
}

func logExecution(playbook string, success bool, output, errorMsg string, startTime, endTime time.Time, duration float64) (int, error) {
	query := `INSERT INTO playbook_logs 
	          (playbook, success, output, error, start_time, end_time, duration) 
	          VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`

	var id int
	err := db.QueryRow(
		query,
		playbook,
		success,
		output,
		errorMsg,
		startTime,
		endTime,
		duration,
	).Scan(&id)

	return id, err
}
