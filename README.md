# Slashoor

A slasher implementation for detecting and submitting attester and proposer slashings on the Ethereum beacon chain.

## Overview

Slashoor detects two types of slashable offenses:

### Attester Slashings

Uses the lazy slasher algorithm with two global functions instead of per-validator databases:

- `m(i)`: minimum target epoch for attestations with source > i
- `M(i)`: maximum target epoch for attestations with source < i

This approach efficiently detects surround votes by checking if `t > m(s)` for any attestation `(s, t)`.

### Proposer Slashings

Detects double proposals where the same validator proposes two different blocks for the same slot:
- **Real-time detection**: Monitors incoming blocks via SSE subscription
- **Historical detection**: Scans Dora explorer for orphaned blocks and compares with canonical blocks

## Slashing Violations Detected

### Attester Violations
1. **Double Vote**: Same validator attests to different block roots for the same target epoch
2. **Surround Vote**: Attestation `(s, t)` surrounds `(s', t')` if `s < s'` AND `t' < t`

### Proposer Violations
3. **Double Proposal**: Same validator proposes two different blocks for the same slot

## Features

- **Multi-node support**: Connect to multiple beacon nodes for redundancy
- **Automatic failover**: API requests failover to healthy nodes
- **Parallel SSE subscriptions**: Subscribe to attestations from all nodes simultaneously
- **Deduplication**: Automatic deduplication of attestations from multiple sources
- **Health tracking**: Nodes are marked unhealthy on failures and retried
- **Dora integration**: Scans Dora explorer for historical double proposals from genesis
- **Validator status checking**: Skips already-slashed validators
- **Header fallback**: Falls back to Dora web scraping when beacon nodes have pruned orphaned blocks

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

proposer:
  enabled: true

submitter:
  enabled: true
  dry_run: false

dora:
  enabled: true
  url: "https://dora.your-network.ethpandaops.io"
  scan_on_startup: true

backfill_slots: 0
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
| `detector.enabled` | Enable attester slashing detection | `true` |
| `proposer.enabled` | Enable proposer slashing detection | `true` |
| `submitter.enabled` | Enable slashing submission | `true` |
| `submitter.dry_run` | Log slashings without submitting | `false` |
| `dora.enabled` | Enable Dora explorer integration | `false` |
| `dora.url` | Dora explorer URL | `""` |
| `dora.scan_on_startup` | Scan for historical double proposals on startup | `true` |
| `backfill_slots` | Number of slots to backfill attestations on startup | `0` |

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
              ┌──────────────┐                          │
              │     Dora     │──────────────────────────┤
              │   Explorer   │                          │
              └──────────────┘                          ▼
                                                ┌─────────────┐
┌─────────────┐     ┌─────────────┐     ┌──────▶│  Submitter  │
│   Indexer   │◀────│ Coordinator │─────┤       │  (submit)   │
│ (m/M algo)  │     │             │     │       └─────────────┘
└──────┬──────┘     └─────────────┘     │
       │                   │            │
       ▼                   ▼            │
┌─────────────┐     ┌─────────────┐     │
│  Detector   │────▶│  Proposer   │─────┘
│ (attester)  │     │  (proposer) │
└─────────────┘     └─────────────┘
```

## Multi-Node Behavior

- **API Requests**: Tried against healthy nodes first, with automatic failover on failure
- **SSE Subscriptions**: All nodes subscribed in parallel for maximum attestation coverage
- **Slashing Submission**: Submitted to all configured nodes for redundancy
- **Health Tracking**: Failed nodes marked unhealthy and retried after 5 seconds

## Dora Integration

When Dora is enabled, slashoor can:

1. **Scan historical double proposals**: On startup, fetches all orphaned blocks from Dora and compares each with its canonical counterpart to detect double proposals
2. **Fetch signed headers**: When beacon nodes have pruned orphaned blocks, falls back to scraping Dora's web interface to get signed block headers with signatures
3. **Check validator status**: Skips validators that are already slashed to avoid redundant submissions

The Dora integration uses page-based pagination to fetch all orphaned blocks from genesis, ensuring no slashable offense is missed.

## License

MIT
