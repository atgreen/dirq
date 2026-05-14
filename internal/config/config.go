// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

// Package config loads DirQ configuration from a YAML-like config file.
// Environment variables override config file values.
//
// Config file format (flat key: value, with tags: block):
//
//	server: grpc.example.com:50051
//	listen: 0.0.0.0:50052
//	exec_enabled: true
//	tags:
//	  env: prod
//	  group: webservers
package config

import (
	"bufio"
	"os"
	"runtime"
	"strings"
)

// File holds parsed config file values.
type File struct {
	Values map[string]string
	Tags   map[string]string
}

// DefaultPath returns the platform-appropriate default config file path.
func DefaultAgentPath() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\dirq\agent.conf`
	}
	return "/etc/dirq/agent.conf"
}

// DefaultServerPath returns the platform-appropriate default server config path.
func DefaultServerPath() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\dirq\server.conf`
	}
	return "/etc/dirq/server.conf"
}

// Load reads a config file. Returns an empty File (not an error) if the
// file doesn't exist, so callers don't need to check existence first.
func Load(path string) (*File, error) {
	f := &File{
		Values: make(map[string]string),
		Tags:   make(map[string]string),
	}

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	defer file.Close()

	inTags := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		// Strip comments.
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Check for indented line (tag entry).
		if inTags && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			key, val := splitKV(trimmed)
			if key != "" {
				f.Tags[key] = val
			}
			continue
		}

		// Top-level key: value.
		inTags = false
		key, val := splitKV(trimmed)
		if key == "" {
			continue
		}

		if key == "tags" && val == "" {
			inTags = true
			continue
		}

		f.Values[key] = val
	}

	return f, scanner.Err()
}

// Get returns the config file value for a key, or empty string.
func (f *File) Get(key string) string {
	if f == nil {
		return ""
	}
	return f.Values[key]
}

// GetTags returns the tags map from the config file.
func (f *File) GetTags() map[string]string {
	if f == nil {
		return nil
	}
	return f.Tags
}

// EnvOr returns the environment variable value if set, then the config file
// value if set, then the fallback.
func EnvOr(env string, fileCfg *File, fileKey string, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	if fileCfg != nil {
		if v := fileCfg.Get(fileKey); v != "" {
			return v
		}
	}
	return fallback
}

func splitKV(s string) (string, string) {
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		// Also try = for compatibility.
		idx = strings.IndexByte(s, '=')
	}
	if idx < 0 {
		return "", ""
	}
	key := strings.TrimSpace(s[:idx])
	val := strings.TrimSpace(s[idx+1:])
	return key, val
}
