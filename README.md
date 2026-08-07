# Slashoor

A production Ethereum beacon-chain slasher: it detects and submits attester
slashings (double and surround votes) and proposer slashings (double proposals),
for both live and historic offenses.

Slashoor uses the same chunked min/max-target detection algorithm as Lighthouse
and Prysm, but offloads durable storage of full attestations to a
[Dora](https://github.com/ethpandaops/dora) explorer — keeping the local index
roughly an order of magnitude smaller. See [`docs/design.md`](docs/design.md)
for the full design.

## What it detects

**Attester slashings** (`is_slashable_attestation_data`):

- **Double vote** — two attestations by the same validator with the same target
  epoch but different data.
- **Surround vote** — one attestation surrounds another (`source_1 < source_2`
  and `target_2 < target_1`), and the reverse (surrounded).

**Proposer slashings** — two distinct blocks by the same validator for the same
slot.

## How it works

- **Detection**: per-validator min/max target arrays, tiled into
  16-epoch × 256-validator compressed chunks in a local [Pebble](https://github.com/cockroachdb/pebble)
  database, giving O(1) surround checks over a sliding window (default 4096
  epochs ≈ 18 days). Double votes and evidence locators use compact per-epoch
  attester records. Only locators are stored locally — never full attestations.
- **Ingestion**: blocks come from Dora (canonical *and* orphaned, historic) and
  from beacon-node SSE (live). Each attestation is expanded to its validator
  indices via committees resolved from Dora duties (historic) or the beacon node
  (recent), including EIP-7549 multi-committee aggregates.
- **Evidence**: on a hit, both attestations are re-fetched, the slashable
  condition re-verified against the spec, the attester sets expanded, and the
  fork-correct `AttesterSlashing` built. Evidence is classified `submittable` vs
  `report-only` (already-slashed intersection, or predating the previous fork so
  BLS domains can no longer verify it).
- **Submission**: fork-aware (`/eth/v2/beacon/pool/attester_slashings` with the
  consensus-version header, v1 for proposer slashings), broadcast to all healthy
  nodes, deduplicated against a persistent marker set.

## Start modes

Configured via `start.mode`:

- `head` — follow the current head; no historic scan.
- `genesis` — scan forward from genesis, then follow head.
- `slot` — scan forward from `start.slot`, then follow head.

The scan resumes from a persisted cursor after a restart. To move the start
point earlier or rebuild, pass `--reset-to-slot N` (0 = genesis), which wipes the
index and rescans.

## Install

```bash
go build -o slashoor .
```

## Usage

```bash
./slashoor --config config.yaml --log-level info
```

See [`config.example.yaml`](config.example.yaml) for all options. Dora must be
enabled (with its blockdb configured) for historic scans and orphaned-block
coverage; without it, slashoor runs on beacon-node data only (live/recent,
canonical-only).

## Dora dependency

Slashoor relies on a few general-purpose Dora API endpoints (branch
`pk910/slashoor-api`):

- `GET /api/v1/epoch/{epoch}/duties` and `GET /api/v1/slot/{slot}/committees` —
  attester committees as global validator indices.
- `POST /api/v1/validators/status` — bulk validator status (plus `slashed` and
  `withdrawable_epoch` on `/api/v1/validators`).

Full block bodies (canonical and orphaned) are fetched via Dora's existing
block-download route.

## License

MIT
