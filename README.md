# Slashoor

A lazy slasher implementation for detecting and submitting attester slashings on the Ethereum beacon chain.

## Overview

Slashoor uses the lazy slasher algorithm with two global functions instead of per-validator databases:

- `m(i)`: minimum target epoch for attestations with source > i
- `M(i)`: maximum target epoch for attestations with source < i

This approach efficiently detects surround votes by checking if `t > m(s)` for any attestation `(s, t)`.

## Slashing Violations Detected

1. **Double Vote**: Same validator attests to different block roots for the same target epoch
2. **Surround Vote**: Attestation `(s, t)` surrounds `(s', t')` if `s < s'` AND `t' < t`

## Features

- **Multi-node support**: Connect to multiple beacon nodes for redundancy
- **Automatic failover**: API requests failover to healthy nodes
- **Parallel SSE subscriptions**: Subscribe to attestations from all nodes simultaneously
- **Deduplication**: Automatic deduplication of attestations from multiple sources
- **Health tracking**: Nodes are marked unhealthy on failures and retried

## Installation

### From Source

```bash
go build -o slashoor .
```

### Docker

```bash
docker build -t slashoor .
```

## Usage

### Configuration

Create a `config.yaml` file:

```yaml
beacon:
  endpoints:
    - "http://localhost:5052"
    - "http://localhost:5053"
    - "http://beacon-node-1:5052"
  timeout: 30s

indexer:
  max_epochs_to_keep: 54000

detector:
  enabled: true

submitter:
  enabled: true
  dry_run: false
```

### Running

```bash
./slashoor --config config.yaml --log-level info
```

### Docker

```bash
docker run -v $(pwd)/config.yaml:/app/config.yaml slashoor --config /app/config.yaml
```

## Configuration Options

| Option | Description | Default |
|--------|-------------|---------|
| `beacon.endpoints` | List of beacon node API URLs | `["http://localhost:5052"]` |
| `beacon.timeout` | HTTP request timeout | `30s` |
| `indexer.max_epochs_to_keep` | Maximum epochs to retain in memory | `54000` |
| `detector.enabled` | Enable slashing detection | `true` |
| `submitter.enabled` | Enable slashing submission | `true` |
| `submitter.dry_run` | Log slashings without submitting | `false` |

## Architecture

```
                         ┌──────────────┐
                         │ Beacon Node 1│───┐
                         └──────────────┘   │
                         ┌──────────────┐   │    ┌─────────────┐
                         │ Beacon Node 2│───┼───▶│   Beacon    │
                         └──────────────┘   │    │   Service   │
                         ┌──────────────┐   │    │ (failover + │
                         │ Beacon Node N│───┘    │   dedup)    │
                         └──────────────┘        └──────┬──────┘
                                                        │
                                                        ▼
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Indexer   │◀────│ Coordinator │────▶│  Detector   │────▶│  Submitter  │
│ (m/M algo)  │     │             │     │   (proof)   │     │  (submit)   │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
```

## Multi-Node Behavior

- **API Requests**: Tried against healthy nodes first, with automatic failover on failure
- **SSE Subscriptions**: All nodes subscribed in parallel for maximum attestation coverage
- **Slashing Submission**: Submitted to all configured nodes for redundancy
- **Health Tracking**: Failed nodes marked unhealthy and retried after 5 seconds

## License

MIT
