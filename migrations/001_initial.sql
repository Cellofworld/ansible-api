-- Создаем отдельную схему для приложения
CREATE SCHEMA IF NOT EXISTS ansible_api;

-- Устанавливаем схему по умолчанию
SET search_path TO ansible_api;

-- Таблица для хранения результатов выполнения
CREATE TABLE IF NOT EXISTS playbook_logs (
    id SERIAL PRIMARY KEY,
    playbook TEXT NOT NULL,
    success BOOLEAN NOT NULL,
    output TEXT,
    error TEXT,
    start_time TIMESTAMP NOT NULL,
    end_time TIMESTAMP NOT NULL,
    duration DOUBLE PRECISION
);

-- Таблица для хранения информации о запусках
CREATE TABLE IF NOT EXISTS playbook_runs (
    id SERIAL PRIMARY KEY,
    playbook TEXT NOT NULL,
    inventory TEXT,
    status VARCHAR(20) NOT NULL,
    start_time TIMESTAMP NOT NULL,
    end_time TIMESTAMP,
    duration DOUBLE PRECISION,
    triggered_by VARCHAR(100),
    extra_vars JSONB,
    output TEXT,
    error TEXT
);

-- Таблица миграций
CREATE TABLE IF NOT EXISTS migrations (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Индексы
CREATE INDEX IF NOT EXISTS idx_playbook_logs_playbook ON playbook_logs(playbook);
CREATE INDEX IF NOT EXISTS idx_playbook_logs_success ON playbook_logs(success);
CREATE INDEX IF NOT EXISTS idx_playbook_logs_start_time ON playbook_logs(start_time);

CREATE INDEX IF NOT EXISTS idx_playbook_runs_playbook ON playbook_runs(playbook);
CREATE INDEX IF NOT EXISTS idx_playbook_runs_status ON playbook_runs(status);
CREATE INDEX IF NOT EXISTS idx_playbook_runs_start_time ON playbook_runs(start_time);