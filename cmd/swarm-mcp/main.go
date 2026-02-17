package main

import (
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/cookchen233/swarm-mcp/internal/mcp"
	"github.com/cookchen233/swarm-mcp/internal/swarm"
)

func main() {
	logger := log.New(os.Stderr, "swarm-mcp: ", log.LstdFlags|log.LUTC)

	// Data root: ~/.swarm-mcp/ by default, override with SWARM_MCP_ROOT
	root := os.Getenv("SWARM_MCP_ROOT")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".swarm-mcp")
	}

	store := swarm.NewStore(root)
	store.EnsureDir()
	store.EnsureDir("docs", "shared")
	store.EnsureDir("issues")
	store.EnsureDir("workers")
	store.EnsureDir("locks", "files")
	store.EnsureDir("locks", "leases")
	store.EnsureDir("trace")

	trace := swarm.NewTraceService(store)

	suggestedMinTaskCount := 0
	if v := os.Getenv("SWARM_MCP_SUGGESTED_MIN_TASK_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			suggestedMinTaskCount = n
		}
	} else if v := os.Getenv("SWARM_MCP_MIN_TASK_COUNT"); v != "" {
		// Backward compatibility
		if n, err := strconv.Atoi(v); err == nil {
			suggestedMinTaskCount = n
		}
	}
	maxTaskCount := 0
	if v := os.Getenv("SWARM_MCP_MAX_TASK_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxTaskCount = n
		}
	}

	issueTTLSec := 3600
	if v := os.Getenv("SWARM_MCP_ISSUE_TTL_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			issueTTLSec = n
		}
	}
	taskTTLSec := 600
	if v := os.Getenv("SWARM_MCP_TASK_TTL_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			taskTTLSec = n
		}
	}

	defaultTimeoutSec := 3600
	if v := os.Getenv("SWARM_MCP_DEFAULT_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			defaultTimeoutSec = n
		}
	}
	if defaultTimeoutSec < 3600 {
		defaultTimeoutSec = 3600
	}

	srv := mcp.NewServer(mcp.ServerConfig{
		Name:                  "swarm-mcp",
		Version:               "0.1.0",
		Logger:                logger,
		Strict:                os.Getenv("SWARM_MCP_STRICT") != "0",
		SuggestedMinTaskCount: suggestedMinTaskCount,
		MaxTaskCount:          maxTaskCount,
		IssueTTLSec:           issueTTLSec,
		TaskTTLSec:            taskTTLSec,
		DefaultTimeoutSec:     defaultTimeoutSec,
	}, store, trace)

	if err := srv.Run(); err != nil {
		logger.Printf("server stopped with error: %v", err)
		os.Exit(1)
	}
}
