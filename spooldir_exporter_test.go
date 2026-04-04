package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestCountFiles(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "spooldir_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test structure
	// tmpDir/file1.txt
	// tmpDir/file2.log
	// tmpDir/subdir/file3.txt
	os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte("test"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "file2.log"), []byte("test"), 0644)
	subdir := filepath.Join(tmpDir, "subdir")
	os.Mkdir(subdir, 0755)
	os.WriteFile(filepath.Join(subdir, "file3.txt"), []byte("test"), 0644)

	c := &Collector{}
	ctx := context.Background()

	tests := []struct {
		name     string
		pattern  string
		maxDepth int
		expected int
	}{
		{"All files recursive", ".*", 0, 3},
		{"Only txt recursive", `.*\.txt$`, 0, 2},
		{"All files no recursion", ".*", 1, 2},
		{"No match", "nonexistent", 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re := regexp.MustCompile(tt.pattern)
			count, _ := c.countFiles(ctx, tmpDir, re, 1, tt.maxDepth)
			if count != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, count)
			}
		})
	}
}

func TestCountFilesSize(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "spooldir_size_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	content1 := "hello"  // 5 bytes
	content2 := "world!" // 6 bytes
	os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte(content1), 0644)
	os.WriteFile(filepath.Join(tmpDir, "file2.txt"), []byte(content2), 0644)

	c := &Collector{}
	ctx := context.Background()
	re := regexp.MustCompile(".*")

	count, size := c.countFiles(ctx, tmpDir, re, 1, 0)
	if count != 2 {
		t.Errorf("expected 2 files, got %d", count)
	}
	if size != 11 {
		t.Errorf("expected 11 bytes, got %d", size)
	}
}

func TestCollector(t *testing.T) {
	tmpDir1, _ := os.MkdirTemp("", "spooldir_test1")
	tmpDir2, _ := os.MkdirTemp("", "spooldir_test2")
	defer os.RemoveAll(tmpDir1)
	defer os.RemoveAll(tmpDir2)

	os.WriteFile(filepath.Join(tmpDir1, "a.txt"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(tmpDir1, "b.txt"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(tmpDir2, "c.txt"), []byte("x"), 0644)

	targets := []Target{
		{Path: tmpDir1, Pattern: ".*"},
		{Path: tmpDir2, Pattern: ".*"},
	}

	collector, err := NewCollector(targets, 0, 5)
	if err != nil {
		t.Fatal(err)
	}

	ch := make(chan prometheus.Metric)
	go func() {
		collector.Collect(ch)
		close(ch)
	}()

	results := make(map[string]float64)
	for m := range ch {
		// Filter by metric name in Desc
		if !strings.Contains(m.Desc().String(), "spooldir_files_count") {
			continue
		}

		var metric dto.Metric
		m.Write(&metric)

		if metric.Gauge != nil {
			path := ""
			pattern := ""
			for _, lp := range metric.Label {
				if *lp.Name == "path" {
					path = *lp.Value
				}
				if *lp.Name == "pattern" {
					pattern = *lp.Value
				}
			}
			if path != "" && pattern != "" {
				results[path+":"+pattern] = *metric.Gauge.Value
			}
		}
	}

	if results[tmpDir1+":.*"] != 2 {
		t.Errorf("expected 2 files in %s, got %f", tmpDir1, results[tmpDir1+":.*"])
	}
	if results[tmpDir2+":.*"] != 1 {
		t.Errorf("expected 1 file in %s, got %f", tmpDir2, results[tmpDir2+":.*"])
	}
}

func TestCollectorTimeout(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "spooldir_timeout")
	defer os.RemoveAll(tmpDir)

	targets := []Target{
		{Path: tmpDir, Pattern: ".*"},
	}

	// Timeout of 0 will trigger immediate deadline
	collector, err := NewCollector(targets, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	ch := make(chan prometheus.Metric)
	go func() {
		collector.Collect(ch)
		close(ch)
	}()

	foundUp := false
	for m := range ch {
		if !strings.Contains(m.Desc().String(), "spooldir_up") {
			continue
		}

		var metric dto.Metric
		m.Write(&metric)

		if metric.Gauge != nil {
			if *metric.Gauge.Value == 0 {
				foundUp = true
			}
		}
	}

	if !foundUp {
		t.Error("expected spooldir_up to be 0 on timeout")
	}
}

func TestCollectorMissingDir(t *testing.T) {
	targets := []Target{
		{Path: "/nonexistent/directory/path/here", Pattern: ".*"},
	}

	collector, err := NewCollector(targets, 0, 5)
	if err != nil {
		t.Fatal(err)
	}

	ch := make(chan prometheus.Metric)
	go func() {
		collector.Collect(ch)
		close(ch)
	}()

	foundDown := false
	for m := range ch {
		if !strings.Contains(m.Desc().String(), "spooldir_up") {
			continue
		}

		var metric dto.Metric
		m.Write(&metric)

		if metric.Gauge != nil && *metric.Gauge.Value == 0 {
			foundDown = true
		}
	}

	if !foundDown {
		t.Error("expected spooldir_up to be 0 for missing directory")
	}
}

func TestCollectorPerTargetMaxDepth(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "spooldir_per_target")
	defer os.RemoveAll(tmpDir)

	os.WriteFile(filepath.Join(tmpDir, "level1.txt"), []byte("x"), 0644)
	subdir := filepath.Join(tmpDir, "subdir")
	os.Mkdir(subdir, 0755)
	os.WriteFile(filepath.Join(subdir, "level2.txt"), []byte("x"), 0644)
	subsubdir := filepath.Join(subdir, "subsubdir")
	os.Mkdir(subsubdir, 0755)
	os.WriteFile(filepath.Join(subsubdir, "level3.txt"), []byte("x"), 0644)

	maxDepth1 := 1
	maxDepth2 := 2
	maxDepth3 := 0 // Infinite

	targets := []Target{
		{Path: tmpDir, Pattern: ".*", MaxDepth: &maxDepth1},
		{Path: tmpDir, Pattern: ".+", MaxDepth: &maxDepth2},
		{Path: tmpDir, Pattern: "^.*$", MaxDepth: &maxDepth3},
	}

	collector, err := NewCollector(targets, 0, 5)
	if err != nil {
		t.Fatal(err)
	}

	ch := make(chan prometheus.Metric)
	go func() {
		collector.Collect(ch)
		close(ch)
	}()

	counts := []float64{}
	for m := range ch {
		if !strings.Contains(m.Desc().String(), "spooldir_files_count") {
			continue
		}

		var metric dto.Metric
		m.Write(&metric)
		if metric.Gauge != nil {
			path := ""
			pattern := ""
			for _, lp := range metric.Label {
				if *lp.Name == "path" {
					path = *lp.Value
				}
				if *lp.Name == "pattern" {
					pattern = *lp.Value
				}
			}
			if path == tmpDir && (pattern == ".*" || pattern == ".+" || pattern == "^.*$") {
				counts = append(counts, *metric.Gauge.Value)
			}
		}
	}

	expected := map[float64]bool{1: false, 2: false, 3: false}
	for _, c := range counts {
		expected[c] = true
	}

	for k, v := range expected {
		if !v {
			t.Errorf("expected count %f not found in results: %v", k, counts)
		}
	}
}

func TestNewCollectorDuplicateTargets(t *testing.T) {
	targets := []Target{
		{Path: "/tmp", Pattern: ".*"},
		{Path: "/tmp/", Pattern: ".*"}, // Duplicate with different suffix
	}

	_, err := NewCollector(targets, 0, 5)
	if err == nil {
		t.Fatal("expected error for duplicate targets, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate target found") {
		t.Errorf("expected error message to contain 'duplicate target found', got: %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "config_test")
	defer os.RemoveAll(tmpDir)

	configPath := filepath.Join(tmpDir, "config.yaml")
	configYAML := `
listen_address: ":9200"
max_depth: 3
targets:
  - path: "/var/log"
    pattern: ".*\\.log"
`
	os.WriteFile(configPath, []byte(configYAML), 0644)

	t.Run("Default values", func(t *testing.T) {
		cfg, err := LoadConfig("", "", []string{"/tmp"}, nil, -1, -1)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ListenAddress != ":9100" {
			t.Errorf("expected :9100, got %s", cfg.ListenAddress)
		}
		if cfg.Timeout != 15 {
			t.Errorf("expected 15, got %d", cfg.Timeout)
		}
	})

	t.Run("Load from file", func(t *testing.T) {
		cfg, err := LoadConfig(configPath, "", nil, nil, -1, -1)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ListenAddress != ":9200" {
			t.Errorf("expected :9200, got %s", cfg.ListenAddress)
		}
		if len(cfg.Targets) != 1 || cfg.Targets[0].Path != "/var/log" {
			t.Errorf("unexpected targets: %+v", cfg.Targets)
		}
	})

	t.Run("CLI overrides file", func(t *testing.T) {
		cfg, err := LoadConfig(configPath, ":9300", []string{"/etc"}, nil, 5, 20)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ListenAddress != ":9300" {
			t.Errorf("expected :9300, got %s", cfg.ListenAddress)
		}
		if cfg.MaxDepth != 5 {
			t.Errorf("expected 5, got %d", cfg.MaxDepth)
		}
		if cfg.Timeout != 20 {
			t.Errorf("expected 20, got %d", cfg.Timeout)
		}
		if len(cfg.Targets) != 1 || cfg.Targets[0].Path != "/etc" {
			t.Errorf("unexpected targets: %+v", cfg.Targets)
		}
	})

	t.Run("Duplicate targets from CLI", func(t *testing.T) {
		_, err := LoadConfig("", "", []string{"/tmp", "/tmp/"}, nil, -1, -1)
		if err == nil {
			t.Fatal("expected error for duplicate targets, got nil")
		}
		if !strings.Contains(err.Error(), "duplicate target found") {
			t.Errorf("expected error message to contain 'duplicate target found', got: %v", err)
		}
	})
}
