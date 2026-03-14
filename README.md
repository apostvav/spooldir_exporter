# Spool Directory Exporter

A Prometheus exporter that counts files in one or more spool directories, with support for regex patterns, recursion depth, and timeouts.

## Installation

```bash
go build -o spooldir_exporter .
```

## Usage

You can configure the exporter using command-line arguments or a configuration file.

### Options

| Flag | Description | Default |
|------|-------------|---------|
| `--web.listen-address` | Address on which to expose metrics. | `:9100` |
| `--config.file` | Path to a YAML configuration file. | (none) |
| `--paths` | Comma-separated list of paths to monitor. | (none) |
| `--file.patterns` | Regex patterns for each path. | `.*` |
| `--max-depth` | Default recursion depth (0 for infinite). | `0` |
| `--timeout` | Timeout in seconds for collecting metrics. | `15` |
| `--version` | Show application version. | - |
| `--help` | Show help message. | - |

**Example:**
```bash
./spooldir_exporter --paths=/var/spool/mail,/tmp --file.patterns='^msg.*','.*' --max-depth=1
```

### Configuration File

The exporter supports a yaml configuration file. Command-line arguments take precedence over the global values defined in the configuration file.

Each target in the `targets` list can optionally specify its own `max_depth`, which will override the global default.

**Example `config.yaml`:**
```yaml
listen_address: ":9100"
max_depth: 2
timeout: 10
targets:
  - path: "/var/spool/mqueue"
    pattern: '.*\.qf'
    max_depth: 1
  - path: "/tmp"
    pattern: '.*\.log'
```

**Running with config file:**
```bash
./spooldir_exporter --config.file=config.yaml
```

## Metrics

The exporter exposes the following metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `spooldir_files_count` | Gauge | Number of files matching the pattern in the specified path. Includes `path` and `pattern` labels. |
| `spooldir_files_size_bytes` | Gauge | Total size of files matching the pattern in the specified path in bytes. Includes `path` and `pattern` labels. |
| `spooldir_up` | Gauge | 1 if the directory was successfully scanned, 0 otherwise (e.g., on timeout). |
| `spooldir_scrape_duration_seconds` | Gauge | Time taken to collect the metrics in seconds. |
| `up` | Gauge | 1 if the exporter itself is running. |
