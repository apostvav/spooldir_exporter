package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"
)

var (
	version = "0.1.0"
)

// Target represents a single directory to monitor.
type Target struct {
	Path     string `yaml:"path"`
	Pattern  string `yaml:"pattern"`
	MaxDepth *int   `yaml:"max_depth"`
}

// Config represents the exporter configuration.
type Config struct {
	ListenAddress string   `yaml:"listen_address"`
	MaxDepth      int      `yaml:"max_depth"`
	Timeout       int      `yaml:"timeout"`
	Targets       []Target `yaml:"targets"`
}

// Collector is a custom collector for spooldir metrics.
type Collector struct {
	paths          []string
	patterns       []*regexp.Regexp
	patternStrings []string
	maxDepths      []int
	timeout        time.Duration

	upDesc             *prometheus.Desc
	spooldirUpDesc     *prometheus.Desc
	filesDesc          *prometheus.Desc
	sizeDesc           *prometheus.Desc
	scrapeDurationDesc *prometheus.Desc
}

// NewCollector returns a new Collector.
func NewCollector(targets []Target, globalMaxDepth int, timeout int) (*Collector, error) {
	paths := make([]string, len(targets))
	compiledPatterns := make([]*regexp.Regexp, len(targets))
	patternStrings := make([]string, len(targets))
	maxDepths := make([]int, len(targets))

	for i, t := range targets {
		paths[i] = t.Path

		pattern := t.Pattern
		if pattern == "" {
			pattern = ".*"
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		compiledPatterns[i] = re
		patternStrings[i] = pattern

		if t.MaxDepth != nil {
			maxDepths[i] = *t.MaxDepth
		} else {
			maxDepths[i] = globalMaxDepth
		}
	}

	commonLabels := []string{"path", "pattern"}

	return &Collector{
		paths:          paths,
		patterns:       compiledPatterns,
		patternStrings: patternStrings,
		maxDepths:      maxDepths,
		timeout:        time.Duration(timeout) * time.Second,
		upDesc: prometheus.NewDesc(
			"up",
			"1 if the exporter is running.",
			nil, nil,
		),
		spooldirUpDesc: prometheus.NewDesc(
			"spooldir_up",
			"1 if the target is up, 0 otherwise.",
			commonLabels, nil,
		),
		filesDesc: prometheus.NewDesc(
			"spooldir_files_count",
			"Number of files in the spool directory.",
			commonLabels, nil,
		),
		sizeDesc: prometheus.NewDesc(
			"spooldir_files_size_bytes",
			"Total size of files in the spool directory in bytes.",
			commonLabels, nil,
		),
		scrapeDurationDesc: prometheus.NewDesc(
			"spooldir_scrape_duration_seconds",
			"metrics scrape duration in seconds",
			nil, nil,
		),
	}, nil
}

// Describe sends the super-set of all possible descriptors of metrics
// collected by this Collector to the provided channel.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.upDesc
	ch <- c.spooldirUpDesc
	ch <- c.filesDesc
	ch <- c.sizeDesc
	ch <- c.scrapeDurationDesc
}

// Collect is called by the Prometheus registry when collecting metrics.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	// Always report up=1 to indicate the exporter itself is running.
	ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, 1)

	type targetResult struct {
		path    string
		pattern string
		count   float64
		size    float64
		up      float64
	}

	// Buffer result channel to avoid blocking if context cancels
	results := make(chan targetResult, len(c.paths))
	var wg sync.WaitGroup

	for i, path := range c.paths {
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			fileCount, fileSize := c.countFiles(ctx, path, c.patterns[i], 1, c.maxDepths[i])

			up := 1.0
			if fileCount == -1 {
				up = 0.0
			}

			// Using select to send or drop if context is done
			select {
			case results <- targetResult{
				path:    path,
				pattern: c.patternStrings[i],
				count:   float64(fileCount),
				size:    float64(fileSize),
				up:      up,
			}:
			case <-ctx.Done():
			}
		}(i, path)
	}

	// Close results channel when all workers are done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Map to track which paths we've received results for
	received := make(map[string]bool)
	collectedResults := []targetResult{}

loop:
	for {
		select {
		case res, ok := <-results:
			if !ok {
				break loop
			}
			received[res.path] = true
			collectedResults = append(collectedResults, res)
		case <-ctx.Done():
			log.Printf("Timeout of %v exceeded", c.timeout)
			break loop
		}
	}

	// Report results we collected
	for _, res := range collectedResults {
		ch <- prometheus.MustNewConstMetric(c.spooldirUpDesc, prometheus.GaugeValue, res.up, res.path, res.pattern)
		if res.up == 1 {
			ch <- prometheus.MustNewConstMetric(c.filesDesc, prometheus.GaugeValue, res.count, res.path, res.pattern)
			ch <- prometheus.MustNewConstMetric(c.sizeDesc, prometheus.GaugeValue, res.size, res.path, res.pattern)
		}
	}

	// For any targets that didn't finish (due to timeout), report them as down
	for i, path := range c.paths {
		if !received[path] {
			ch <- prometheus.MustNewConstMetric(c.spooldirUpDesc, prometheus.GaugeValue, 0, path, c.patternStrings[i])
		}
	}

	ch <- prometheus.MustNewConstMetric(c.scrapeDurationDesc, prometheus.GaugeValue, time.Since(start).Seconds())
}

func (c *Collector) countFiles(ctx context.Context, path string, pattern *regexp.Regexp, depth int, maxDepth int) (int, int64) {
	if maxDepth != 0 && depth > maxDepth {
		return 0, 0
	}

	// Check context early
	if ctx.Err() != nil {
		return -1, -1
	}

	files, err := os.ReadDir(path)
	if err != nil {
		log.Printf("Error reading directory %s: %v", path, err)
		return -1, -1
	}

	fileCount := 0
	var totalSize int64 = 0
	for _, file := range files {
		if ctx.Err() != nil {
			return -1, -1
		}

		if file.IsDir() {
			count, size := c.countFiles(ctx, filepath.Join(path, file.Name()), pattern, depth+1, maxDepth)
			if count == -1 {
				return -1, -1
			}
			fileCount += count
			totalSize += size
		} else {
			if pattern.MatchString(file.Name()) {
				info, err := file.Info()
				if err != nil {
					log.Printf("Error getting file info for %s: %v", file.Name(), err)
					continue
				}
				// Check for regular files (no symlinks, no directories etc)
				if info.Mode().IsRegular() {
					fileCount++
					totalSize += info.Size()
				}
			}
		}
	}
	return fileCount, totalSize
}

// LoadConfig loads the configuration from a file and overrides it with CLI flags.
func LoadConfig(configFile string, listenAddress string, paths []string, patterns []string, maxDepth int, timeout int) (*Config, error) {
	// Default configuration
	cfg := &Config{
		ListenAddress: ":9100",
		MaxDepth:      0,
		Timeout:       15,
	}

	if configFile != "" {
		f, err := os.Open(configFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if err := yaml.NewDecoder(f).Decode(cfg); err != nil {
			return nil, err
		}
	}

	// CLI flags override everything
	if listenAddress != "" {
		cfg.ListenAddress = listenAddress
	}
	if maxDepth != -1 {
		cfg.MaxDepth = maxDepth
	}
	if timeout != -1 {
		cfg.Timeout = timeout
	}

	// If CLI paths are provided, they override the targets from the config file.
	if len(paths) > 0 {
		if len(patterns) > len(paths) {
			return nil, fmt.Errorf("the number of patterns (%d) cannot be greater than the number of paths (%d)", len(patterns), len(paths))
		}

		if len(patterns) > 1 && len(patterns) < len(paths) {
			return nil, fmt.Errorf("mismatch between the number of paths (%d) and patterns (%d)", len(paths), len(patterns))
		}

		processedPatterns := patterns
		if len(processedPatterns) == 0 {
			processedPatterns = []string{".*"}
		}

		if len(processedPatterns) == 1 && len(paths) > 1 {
			p := processedPatterns[0]
			for i := 1; i < len(paths); i++ {
				processedPatterns = append(processedPatterns, p)
			}
		}

		cfg.Targets = nil
		for i, path := range paths {
			cfg.Targets = append(cfg.Targets, Target{
				Path:    path,
				Pattern: processedPatterns[i],
			})
		}
	}

	if len(cfg.Targets) == 0 {
		return nil, fmt.Errorf("no targets defined, use --paths or --config.file")
	}

	// Check for duplicate targets (same path and pattern)
	seen := make(map[string]bool)
	for _, t := range cfg.Targets {
		p := t.Pattern
		if p == "" {
			p = ".*"
		}
		key := fmt.Sprintf("%s|%s", t.Path, p)
		if seen[key] {
			return nil, fmt.Errorf("duplicate target found: path=%s, pattern=%s", t.Path, p)
		}
		seen[key] = true
	}

	return cfg, nil
}

func main() {
	var (
		configFile    = kingpin.Flag("config.file", "Path to configuration file.").String()
		listenAddress = kingpin.Flag("web.listen-address", "Address on which to expose metrics and web interface.").String()
		paths         = kingpin.Flag("paths", "A list of paths to watch for files.").Strings()
		patterns      = kingpin.Flag("file.patterns", "A list of regex patterns to match files against.").Strings()
		maxDepth      = kingpin.Flag("max-depth", "Maximum depth to scan for files.").Default("-1").Int()
		timeout       = kingpin.Flag("timeout", "Timeout in seconds for collecting metrics.").Default("-1").Int()
	)

	kingpin.Version(version)
	kingpin.HelpFlag.Short('h')

	kingpin.Parse()

	cfg, err := LoadConfig(*configFile, *listenAddress, *paths, *patterns, *maxDepth, *timeout)
	if err != nil {
		log.Fatal(err)
	}

	collector, err := NewCollector(cfg.Targets, cfg.MaxDepth, cfg.Timeout)
	if err != nil {
		log.Fatalf("Error creating collector: %v", err)
	}

	prometheus.MustRegister(collector)

	http.Handle("/metrics", promhttp.Handler())
	log.Printf("Starting spooldir_exporter v%s on %s", version, cfg.ListenAddress)
	log.Fatal(http.ListenAndServe(cfg.ListenAddress, nil))
}
