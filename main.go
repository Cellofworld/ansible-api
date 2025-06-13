package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"ansible-api/config"
)

// Модели для GORM
type PlaybookRequest struct {
	Playbook  string            `json:"playbook"`
	Inventory string            `json:"inventory,omitempty"`
	ExtraVars map[string]string `json:"extra_vars,omitempty" gorm:"-"`
}

type PlaybookLog struct {
	gorm.Model
	Playbook  string    `gorm:"type:text;not null" json:"playbook"`
	Success   bool      `gorm:"type:boolean;not null" json:"success"`
	Output    string    `gorm:"type:text" json:"output"`
	Error     string    `gorm:"type:text" json:"error"`
	StartTime time.Time `gorm:"type:timestamptz;not null" json:"start_time"`
	EndTime   time.Time `gorm:"type:timestamptz;not null" json:"end_time"`
	Duration  float64   `gorm:"type:decimal;not null" json:"duration"`
}

type PlaybookRunStatus string

const (
	RunStatusStarted   PlaybookRunStatus = "started"
	RunStatusCompleted PlaybookRunStatus = "completed"
	RunStatusFailed    PlaybookRunStatus = "failed"
)

type PlaybookRun struct {
	gorm.Model
	Playbook    string            `gorm:"type:text;not null" json:"playbook"`
	Inventory   string            `gorm:"type:text" json:"inventory,omitempty"`
	Status      PlaybookRunStatus `gorm:"type:text;not null" json:"status"`
	StartTime   time.Time         `gorm:"type:timestamptz;not null" json:"start_time"`
	EndTime     *time.Time        `gorm:"type:timestamptz" json:"end_time,omitempty"`
	Duration    *float64          `gorm:"type:decimal" json:"duration,omitempty"`
	TriggeredBy string            `gorm:"type:text" json:"triggered_by,omitempty"`
	ExtraVars   JSONMap           `gorm:"type:jsonb" json:"extra_vars,omitempty"`
	Output      string            `gorm:"type:text" json:"output,omitempty"`
	Error       string            `gorm:"type:text" json:"error,omitempty"`
}

// JSONMap для работы с JSONB в PostgreSQL
type JSONMap map[string]string

func (j *JSONMap) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	b, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("value is not []byte")
	}
	return json.Unmarshal(b, &j)
}

func (j JSONMap) Value() (interface{}, error) {
	if j == nil {
		return nil, nil
	}
	return json.Marshal(j)
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
	db      *gorm.DB
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

	// Автомиграции - создание таблиц
	if err := db.AutoMigrate(&PlaybookRun{}, &PlaybookLog{}); err != nil {
		log.Fatalf("Failed to auto-migrate database: %v", err)
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
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s search_path=ansible_api,public",
		cfg.Database.Host,
		cfg.Database.Port,
		cfg.Database.User,
		cfg.Database.Password,
		cfg.Database.Name,
		cfg.Database.SSLMode,
	)

	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
		NamingStrategy: schema.NamingStrategy{
			TablePrefix:   "ansible_api.", // Все таблицы будут созданы в схеме ansible_api
			SingularTable: true,
		},
	})
	if err != nil {
		return err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return err
	}

	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(25)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	return nil
}

func cleanupOldLogs() {
	log.Printf("Starting cleanup of logs older than %d days", cfg.Logging.RetentionDays)

	retentionPeriod := time.Now().AddDate(0, 0, -cfg.Logging.RetentionDays)

	// Удаление старых логов
	result := db.Where("start_time < ?", retentionPeriod).Delete(&PlaybookLog{})
	if result.Error != nil {
		log.Printf("Error cleaning up old logs: %v", result.Error)
		return
	}

	// Удаление старых запусков
	result = db.Where("start_time < ?", retentionPeriod).Delete(&PlaybookRun{})
	if result.Error != nil {
		log.Printf("Error cleaning up old runs: %v", result.Error)
		return
	}

	log.Printf("Cleaned up %d old log entries", result.RowsAffected)
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

		// Логирование выполнения
		success := err == nil
		errorMsg := ""
		if err != nil {
			errorMsg = err.Error()
		}
		_ = logExecution(req.Playbook, success, output, errorMsg, startTime, endTime, duration)

		// Обновление статуса запуска
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

	query := db.Model(&PlaybookLog{})

	if successFilter != "" {
		success, err := strconv.ParseBool(successFilter)
		if err == nil {
			query = query.Where("success = ?", success)
		}
	}

	if playbookFilter != "" {
		query = query.Where("playbook = ?", playbookFilter)
	}

	if dateFrom != "" {
		if fromTime, err := time.Parse(time.RFC3339, dateFrom); err == nil {
			query = query.Where("start_time >= ?", fromTime)
		}
	}

	if dateTo != "" {
		if toTime, err := time.Parse(time.RFC3339, dateTo); err == nil {
			query = query.Where("start_time <= ?", toTime)
		}
	}

	var totalCount int64
	if err := query.Count(&totalCount).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	totalPages := (int(totalCount) + cfg.Logging.PageSize - 1) / cfg.Logging.PageSize
	if page > totalPages && totalPages > 0 {
		page = totalPages
	}
	offset := (page - 1) * cfg.Logging.PageSize

	var logs []PlaybookLog
	if err := query.Order("start_time DESC").
		Limit(cfg.Logging.PageSize).
		Offset(offset).
		Find(&logs).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := LogsResponse{
		Logs:        logs,
		TotalCount:  int(totalCount),
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
	if err := db.First(&logEntry, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
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

	query := db.Model(&PlaybookRun{})

	if statusFilter != "" {
		query = query.Where("status = ?", statusFilter)
	}

	if playbookFilter != "" {
		query = query.Where("playbook = ?", playbookFilter)
	}

	if dateFrom != "" {
		if fromTime, err := time.Parse(time.RFC3339, dateFrom); err == nil {
			query = query.Where("start_time >= ?", fromTime)
		}
	}

	if dateTo != "" {
		if toTime, err := time.Parse(time.RFC3339, dateTo); err == nil {
			query = query.Where("start_time <= ?", toTime)
		}
	}

	var totalCount int64
	if err := query.Count(&totalCount).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	totalPages := (int(totalCount) + cfg.Logging.PageSize - 1) / cfg.Logging.PageSize
	if page > totalPages && totalPages > 0 {
		page = totalPages
	}
	offset := (page - 1) * cfg.Logging.PageSize

	var runs []PlaybookRun
	if err := query.Order("start_time DESC").
		Limit(cfg.Logging.PageSize).
		Offset(offset).
		Find(&runs).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := RunsResponse{
		Runs:        runs,
		TotalCount:  int(totalCount),
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
	if err := db.First(&run, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			http.Error(w, "Run not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run)
}

func logPlaybookStart(req PlaybookRequest, remoteAddr string) (uint, error) {
	run := PlaybookRun{
		Playbook:    req.Playbook,
		Inventory:   req.Inventory,
		Status:      RunStatusStarted,
		StartTime:   time.Now(),
		TriggeredBy: remoteAddr,
		ExtraVars:   req.ExtraVars,
	}

	if err := db.Create(&run).Error; err != nil {
		return 0, err
	}

	return run.ID, nil
}

func updatePlaybookRun(runID uint, status PlaybookRunStatus, output, errorMsg string) error {
	updates := map[string]interface{}{
		"status": status,
		"output": output,
		"error":  errorMsg,
	}

	if status != RunStatusStarted {
		endTime := time.Now()
		var startTime time.Time
		if err := db.Model(&PlaybookRun{}).Where("id = ?", runID).Pluck("start_time", &startTime).Error; err != nil {
			return err
		}
		duration := endTime.Sub(startTime).Seconds()

		updates["end_time"] = endTime
		updates["duration"] = duration
	}

	return db.Model(&PlaybookRun{}).Where("id = ?", runID).Updates(updates).Error
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

func logExecution(playbook string, success bool, output, errorMsg string, startTime, endTime time.Time, duration float64) error {
	logEntry := PlaybookLog{
		Playbook:  playbook,
		Success:   success,
		Output:    output,
		Error:     errorMsg,
		StartTime: startTime,
		EndTime:   endTime,
		Duration:  duration,
	}

	return db.Create(&logEntry).Error
}
