package dgettenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

type Key struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Default  string `json:"default,omitempty"`
}

type Report struct {
	EnvFile    string   `json:"env_file"`
	MissingEnv []string `json:"missing_env"`
	Keys       []Key    `json:"keys"`
}

type Env struct {
	file    string
	missing []string
	keys    []Key
	seen    map[string]bool
}

func Load() *Env {
	file := os.Getenv("DGETT_ENV")
	if file == "" {
		file = ".env"
	}
	if err := godotenv.Load(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "env file %s: %v\n", file, err)
	}
	return &Env{file: file, seen: map[string]bool{}}
}

func (e *Env) record(k Key) {
	if !e.seen[k.Name] {
		e.seen[k.Name] = true
		e.keys = append(e.keys, k)
	}
}

func (e *Env) Require(key string) string {
	return e.RequireOr(key, "")
}

func (e *Env) RequireOr(key, fallback string) string {
	e.record(Key{Name: key, Required: fallback == ""})
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return os.Getenv(key)
	}
	if fallback != "" {
		return fallback
	}
	e.missing = append(e.missing, key)
	return ""
}

func (e *Env) Default(key, fallback string) string {
	e.record(Key{Name: key, Default: fallback})
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (e *Env) Missing() []string {
	return e.missing
}

func (e *Env) Check() {
	if path := os.Getenv("DGETT_REPORT"); path != "" {
		data, _ := json.Marshal(Report{EnvFile: e.file, MissingEnv: e.missing, Keys: e.keys})
		os.WriteFile(path, data, 0o644)
	}
	if len(e.missing) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "env file %s is missing: %s\n", e.file, strings.Join(e.missing, ", "))
	os.Exit(1)
}
