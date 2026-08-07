# Slashoor Design

Standalone Ethereum beacon chain slasher. Detects and submits attester slashings (double
votes, surround votes) and proposer slashings (double proposals), for both live and
historic offenses. Uses [Dora](https://github.com/ethpandaops/dora) as the durable
attestation/block store and beacon nodes for live data and submission.

## Slashable conditions (consensus specs)

Attester (`is_slashable_attestation_data`, unchanged phase0 → Gloas):

1. **Double vote**: `data_1 != data_2 && data_1.target.epoch == data_2.target.epoch`.
   Equivalent to comparing `hash_tree_root(AttestationData)`.
2. **Surround vote**: `data_1.source.epoch < data_2.source.epoch && data_2.target.epoch <
   data_1.target.epoch` (att_1 surrounds att_2). Strict inequalities, directional — the
   submitted pair must be ordered so attestation_1 surrounds attestation_2.

Proposer (`process_proposer_slashing`): two `SignedBeaconBlockHeader` with equal slot and
proposer index but different header content. The signature covers the header, so signed
headers are sufficient evidence.

Submission constraints that shape the design:

- `process_attester_slashing` asserts `slashed_any`: at least one validator in the
  intersection of both attesting sets must not be slashed yet — an all-slashed
  intersection makes the whole operation **invalid**, not a no-op. Filter before submit.
- BLS domains: `get_domain` can only reconstruct `state.fork.current_version` and
  `previous_version`. Evidence signed under an older fork version can never validate
  again. Offenses older than the previous fork epoch are **reportable but not
  submittable**.
- Electra (EIP-7549): `AttestationData.index == 0`, committees selected via
  `committee_bits`, `aggregation_bits` concatenated over committees in committee order,
  `IndexedAttestation.attesting_indices` max 131,072, `MAX_ATTESTER_SLASHINGS_ELECTRA =
  1` per block. Attester slashings submit via
  `POST /eth/v2/beacon/pool/attester_slashings` with mandatory `Eth-Consensus-Version`
  header (v1 route removed in beacon-APIs v4.0.0). Proposer slashings stay on v1.
- Gloas changes SSZ shapes only (progressive containers); no new offenses. We submit
  JSON and only hash `AttestationData` (fork-invariant), so impact is minimal.
- The evidence pair must carry the **full attesting sets with the original aggregate
  signatures** — submitting only the overlap fails `is_valid_indexed_attestation`.

## Detection algorithm: chunked min-max targets

Same algorithm and constants as Lighthouse (`slasher` crate) and Prysm
(`beacon-chain/slasher`), which independently converged on it (design doc:
https://hackmd.io/@sproul/min-max-slasher):

- `chunkSize C = 16` epochs, `validatorChunkSize K = 256` validators,
  `historyLength H = 4096` epochs default (config; multiple of C; ≤ 65536 — the `uint16`
  distance ceiling, ~291 days).
- Per validator, over a sliding H-epoch window, two arrays of `uint16` **distances**
  (`target - epoch`, saturating at `MaxUint16`):
  - `minTargets[v][e]` = min target of any attestation by `v` with `source > e`.
    A new attestation `(s,t)` **surrounds** an existing one iff `t > minTargets[v][s]`.
  - `maxTargets[v][e]` = max target with `source < e`. New att **is surrounded** iff
    `t < maxTargets[v][s]`. Both checks are O(1).
- Updates: min propagates backward from `s-1`, max forward from `s+1`, each with early
  termination when the invariant already holds, hopping across chunk boundaries.
- **Double votes**: a separate per-`(validator, targetEpoch)` record holding the
  attestation data root — a different root at the same target is a double vote.

The (epochs × validators) matrix is tiled into `C × K` chunks (one DB value each), so a
single aggregate attestation covering hundreds of validators is applied with one
read/modify/write per validator-chunk rather than per validator.

### Detection horizon

A surround pair is only found while the surrounder's source epoch is still inside the
`H`-epoch window when the second attestation is processed. On-chain attestations have
`target` within ~2 epochs of inclusion, so a pair only spans more than `H` epochs during
prolonged non-finality. `H = 4096` (~18 days of non-finality) exceeds anything mainnet has
seen and sits above the Electra weak-subjectivity period (~3532 epochs). Genesis *scans*
are supported; a genesis-sized *window* is not (u16 ceiling) — the window is the
surround-detection horizon, documented as such.

## Slashoor's twist: Dora is the durable store

Lighthouse/Prysm persist every full `IndexedAttestation` (indices + aggregate signature)
solely to rebuild proofs on a hit — the dominant cost. Slashoor indexes **block-included
attestations only** (canonical + orphaned, both served by Dora), so every indexed
attestation is re-fetchable. The local DB stores only the detection index plus a
**locator** `(block_root, attestation_index_in_block)`. Gossip-only attestations that
never land in a block are out of scope (v1).

## Local index: Pebble

Single embedded [Pebble](https://github.com/cockroachdb/pebble) instance (pure-Go, block
compression, cheap range-delete pruning). No server, no SQL. One-byte key prefixes,
big-endian keys for ordered iteration and prefix range-deletes. Absolute epoch keys (no
`epoch % H` ring buffer — range-deletes make it unnecessary).

| Prefix | Key | Value |
|--------|-----|-------|
| `m` | fixed | metadata: schema version, `C/K/H`, cursor `(startSlot, processedSlot)`, fork schedule |
| `n` / `x` | `echunk(4) ‖ vchunk(4)` | min / max target chunk: `uint16[C*K]` distances (Pebble-compressed) |
| `d` | `epoch(4)` | epoch dict: entries `dataRoot8(8) ‖ blockRoot(32) ‖ slot(4) ‖ attIndex(2)` — one per distinct `AttestationData` seen at that target epoch |
| `a` | `epoch(4) ‖ vchunk(4)` | attester group: `K` refs into that epoch's dict (varint list); ref 0 = didn't attest |
| `p` | `slot(8) ‖ proposer(5)` | header root (32) — proposer double-proposal check |
| `s` | offense id | submitted / observed marker |
| `v` | `validatorIndex(5)` | slashed marker (local mirror of on-chain slashed set) |

Data roots are stored truncated to 8 bytes: on any apparent conflict both attestations are
re-fetched from Dora and `is_slashable_attestation_data` is re-checked before submit, so a
truncation collision costs a wasted lookup, never a false report.

Epoch-keyed keyspaces (`n`,`x` by `echunk`, `d`,`a` by `epoch`) prune with one range-delete
per departed chunk-window. Chunk writes are batched in an in-memory dirty cache flushed on
epoch cadence (live) or chunk-width alignment (backfill); all updates are idempotent, so a
crash re-processes from the cursor with no corruption.

### Expected size

Raw ≈ `5 bytes × validators × H` (4 for the two chunk arrays + ~1 for attester refs).
Snappy typically takes 4–8× off. Mainnet (~2M indices, H=4096): ~41 GB raw → ~5–10 GB on
disk. Large devnet (500k): ~1–3 GB. Small devnet: tens of MB. The DB is disposable —
`--reset-to-slot` re-scans from Dora.

## Package layout

```
pkg/
  chainspec/   fork schedule, /eth/v1/config/spec, dynssz, domain/version helpers
  beacon/      multi-node CL client: events, block-by-root, committees, submit
  dora/        Dora API+download client: slots, block SSZ, epoch duties, validator status
  duties/      epoch committee resolution (dora primary, beacon fallback) + bit expansion
  indexdb/     Pebble store, chunk math, pruning, cursor
  slasher/     chunked min-max detection + double-vote + proposer, batch processor
  evidence/    candidate -> refetch -> build & classify AttesterSlashing/ProposerSlashing
  submitter/   dedup, dry-run, fork-aware submission, retry, metrics
  coordinator/ start modes, backfill sweep, live follow, pruning, wiring
cmd/            cobra CLI
```

## Indexer modes & lifecycle

- **Start mode** (`start: head | genesis | <slot>`): persisted as cursor
  `(startSlot, processedSlot)`.
  - `head`: begin live-following at current head; no historic scan.
  - `genesis` / `<slot>`: forward chronological sweep from `startSlot` via Dora
    (canonical + orphaned), then hand off to live SSE following at head.
- **Resume**: cursor advances per processed batch; a killed backfill resumes at
  `processedSlot`. Idempotent updates make reprocessing safe.
- **Reset**: explicit `--reset-to-slot N` (wipe DB + new cursor). If the configured start
  moves *earlier* than the stored start without `--reset`, slashoor refuses to start
  (detection state from a later start is incomplete). Moving start *later* needs no reset.
- **Pruning**: finality-driven; drop chunk-windows and epoch dicts below
  `currentEpoch - H`.

## Ingestion pipeline (per-slot batch)

1. Source blocks: live SSE `block` events (fetch by root — works for orphaned too);
   backfill from Dora slot list + block download. Persisted cursor for resume.
2. Expand each attestation to validator indices via `duties` (beacon committees for recent
   epochs, Dora epoch-duties for historic; per-epoch cache; EIP-7549 committee-offset
   walk).
3. Filter: drop `source > target`, older than window / previous fork; re-queue future
   targets; dedup by data root within batch.
4. Group by validator chunk; per chunk run double-vote + min/max surround checks, apply
   updates, flush dirty chunks.

## Hit path

Chunk flags the conflicting target epoch → attester record gives data root + locator →
fetch both blocks (Dora/beacon) → expand attesting sets → build the `IndexedAttestation`
pair (full sets, original aggregate signatures), order surrounder first → re-check
`is_slashable_attestation_data` → drop already-slashed-only intersections (local `v` set +
Dora `/v1/slashings` + validator status) → classify submittable vs report-only (fork
domain) → dedup against `s` markers → submit fork-aware, else report.

## Dora dependency & API extensions

Everything slashoor reads already exists inside Dora; the block download route already
serves full `SignedBeaconBlock` SSZ by root (orphaned included). Two general-purpose API
additions (branch `pk910/slashoor-api`) close the remaining gaps:

1. `GET /api/v1/epoch/{epoch}/duties` and `GET /api/v1/slot/{slot}/committees` — attester
   committees as global validator indices (wrap `ChainService.GetSlotCommittees` /
   blockdb `EpochDuties`; no beacon node needed).
2. `POST /api/v1/validators/status` (bulk) + `slashed`/`withdrawable_epoch` fields on
   `/v1/validators` — for the `slashed_any` filter.

Operational contract: Dora must run with blockdb configured (finalized canonical bodies
and duties live only there) and retain history for ≥ the slasher window. Slashoor degrades
to report-only when a locator or duty points at pruned data.
