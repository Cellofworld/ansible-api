package config

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ilyakaznacheev/cleanenv"
)

type Config struct {
	Server   `yaml:"server"`
	Database `yaml:"database"`
	Logging  `yaml:"logging"`
	Ansible  `yaml:"ansible"`
}

type Server struct {
	Port         string        `yaml:"port" env:"PORT" env-default:"8080"`
	PlaybooksDir string        `yaml:"playbooks_dir" env:"PLAYBOOKS_DIR" env-default:"./playbooks"`
	ReadTimeout  time.Duration `yaml:"read_timeout" env:"READ_TIMEOUT" env-default:"10s"`
	WriteTimeout time.Duration `yaml:"write_timeout" env:"WRITE_TIMEOUT" env-default:"10s"`
}

type Database struct {
	Host     string `yaml:"host" env:"DB_HOST" env-default:"localhost"`
	Port     int    `yaml:"port" env:"DB_PORT" env-default:"5432"`
	User     string `yaml:"user" env:"DB_USER" env-default:"ansible"`
	Password string `yaml:"password" env:"DB_PASSWORD" env-default:"password"`
	Name     string `yaml:"name" env:"DB_NAME" env-default:"ansible_logs"`
	SSLMode  string `yaml:"ssl_mode" env:"DB_SSLMODE" env-default:"disable"`
}

type Logging struct {
	RetentionDays int `yaml:"retention_days" env:"LOG_RETENTION_DAYS" env-default:"30"`
	PageSize      int `yaml:"page_size" env:"LOG_PAGE_SIZE" env-default:"20"`
}

type Ansible struct {
	Timeout       int    `yaml:"timeout" env:"ANSIBLE_TIMEOUT" env-default:"3600"`
	DefaultPython string `yaml:"default_python" env:"ANSIBLE_PYTHON" env-default:"/usr/bin/python3"`
}

func Load() (*Config, error) {
	cfg := &Config{}

	configPaths := []string{
		"./config.yml",
		"./config/config.yml",
		"/etc/ansible-api/config.yml",
	}

	var configFile string
	for _, path := range configPaths {
		if _, err := os.Stat(path); err == nil {
			configFile = path
			break
		}
	}

	if configFile != "" {
		if err := cleanenv.ReadConfig(configFile, cfg); err != nil {
			return nil, fmt.Errorf("config file error: %v", err)
		}
	} else {
		log.Println("Config file not found, using environment variables only")
	}

	if err := cleanenv.ReadEnv(cfg); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(cfg.Server.PlaybooksDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create playbooks directory: %v", err)
	}

	return cfg, nil
}
