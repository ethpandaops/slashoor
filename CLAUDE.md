# Slashoor

Ethereum beacon chain slasher for detecting and submitting attester and proposer slashings.

## Pre-commit Checks

Before committing changes, always run these checks:

```bash
# Format code
gofmt -s -w .

# Run vet
go vet ./...

# Run staticcheck
staticcheck ./...

# Run linter
golangci-lint run ./...

# Run tests with race detector
go test -race ./...

# Build
go build ./...
```

Or run all checks in one command:

```bash
gofmt -s -w . && go vet ./... && staticcheck ./... && go test -race ./... && go build ./...
```

## Project Structure

See `docs/design.md` for the full architecture (spec conditions, the chunked
min/max detection algorithm, the Pebble index layout, and the Dora dependency).

```
slashoor/
├── main.go                 # Entry point
├── cmd/                    # CLI commands (cobra)
├── docs/design.md          # Architecture & design reference
├── pkg/
│   ├── chainspec/         # Chain spec: fork schedule, slot/epoch math, dynssz
│   ├── beacon/            # Beacon node client (multi-node, failover, SSE, submit)
│   ├── dora/              # Dora client: block download, epoch duties, validator status
│   ├── duties/            # Committee resolution + EIP-7549 attestation expansion
│   ├── indexer/           # Pebble index + chunked min/max detection engine
│   ├── ingest/            # Block -> detection inputs (votes, proposal, on-chain slashings)
│   ├── detector/          # Evidence builder: re-fetch, verify, classify, build slashings
│   ├── submitter/         # Fork-aware submission, dedup, dry-run
│   └── coordinator/       # Orchestrates backfill + live following, wiring, pruning
```

The Dora API extensions this project depends on live on the `pk910/slashoor-api`
branch of the Dora repo (`/api/v1/epoch/{epoch}/duties`,
`/api/v1/slot/{slot}/committees`, `/api/v1/validators/status`, plus `slashed` /
`withdrawable_epoch` on `/api/v1/validators`).

## Configuration

See `config.example.yaml` for configuration options. Key settings:

- `beacon.endpoints` - Beacon node URLs (read failover, submission broadcast)
- `dora.enabled` / `dora.url` - Durable block/duty store (needed for historic & orphaned coverage)
- `indexer.history_length` - Surround-detection window in epochs (default 4096, max 65536)
- `start.mode` - `head` | `genesis` | `slot` (with `start.slot`)
- `submitter.dry_run` - Log slashings without submitting
- `--reset-to-slot N` - Wipe the index and rescan from slot N (0 = genesis)

## Running

```bash
./slashoor --config config.yaml --log-level info
```
