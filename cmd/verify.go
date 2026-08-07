package cmd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/ethpandaops/slashoor/pkg/beacon"
	"github.com/ethpandaops/slashoor/pkg/chainspec"
	"github.com/ethpandaops/slashoor/pkg/detector"
	"github.com/ethpandaops/slashoor/pkg/dora"
	"github.com/ethpandaops/slashoor/pkg/duties"
	"github.com/ethpandaops/slashoor/pkg/indexer"
)

var (
	vSlot1, vSlot2 uint64
	vRoot1, vRoot2 string
	vIdx1, vIdx2   uint
	vDoSubmit      bool
)

// verifyCmd builds — and optionally submits — a single attester slashing for a
// specific pair of block-included attestations, to verify a detection against a
// live network. It reuses slashoor's real evidence builder and submitter.
var verifyCmd = &cobra.Command{
	Use:   "submit-slashing",
	Short: "Build and (with --submit) submit one attester slashing for a specific attestation pair",
	RunE:  runVerify,
}

func init() {
	verifyCmd.Flags().Uint64Var(&vSlot1, "slot1", 0, "inclusion slot of attestation 1")
	verifyCmd.Flags().StringVar(&vRoot1, "root1", "", "block root of attestation 1 (0x…)")
	verifyCmd.Flags().UintVar(&vIdx1, "idx1", 0, "attestation index within block 1")
	verifyCmd.Flags().Uint64Var(&vSlot2, "slot2", 0, "inclusion slot of attestation 2")
	verifyCmd.Flags().StringVar(&vRoot2, "root2", "", "block root of attestation 2 (0x…)")
	verifyCmd.Flags().UintVar(&vIdx2, "idx2", 0, "attestation index within block 2")
	verifyCmd.Flags().BoolVar(&vDoSubmit, "submit", false, "actually submit the slashing to the network")
	rootCmd.AddCommand(verifyCmd)
}

func parseRootFlag(s string) (phase0.Root, error) {
	var r phase0.Root
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return r, err
	}
	if len(b) != 32 {
		return r, fmt.Errorf("root %q is %d bytes, want 32", s, len(b))
	}
	copy(r[:], b)
	return r, nil
}

func runVerify(cmd *cobra.Command, _ []string) error {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	cfg, err := loadConfig(cfgFile)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	root1, err := parseRootFlag(vRoot1)
	if err != nil {
		return fmt.Errorf("root1: %w", err)
	}
	root2, err := parseRootFlag(vRoot2)
	if err != nil {
		return fmt.Errorf("root2: %w", err)
	}

	ctx := context.Background()

	beaconSvc := beacon.New(cfg.Beacon, log)
	if err := beaconSvc.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = beaconSvc.Stop() }()

	rawSpec, err := beaconSvc.Spec(ctx)
	if err != nil {
		return fmt.Errorf("fetch spec: %w", err)
	}
	spec, err := chainspec.New(rawSpec)
	if err != nil {
		return err
	}

	var doraClient dora.Client
	if cfg.Dora.Enabled {
		doraClient = dora.New(cfg.Dora, log)
		doraClient.SetSpec(spec)
	}

	tmpDir, err := os.MkdirTemp("", "slashoor-verify-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	store, err := indexer.Open(tmpDir, indexer.DefaultConfig(), log)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	dutiesResolver := duties.New(beaconSvc, doraClient, spec, log)
	evidence := detector.New(&detector.Config{Enabled: true}, spec, beaconSvc, doraClient, dutiesResolver, store, log)

	fin, err := beaconSvc.Finality(ctx)
	if err != nil {
		return fmt.Errorf("fetch head: %w", err)
	}
	headEpoch := spec.EpochOfSlot(fin.HeadSlot)

	cand := indexer.SlashingCandidate{
		Type: indexer.ViolationSurroundVote,
		Att1: indexer.Locator{Slot: phase0.Slot(vSlot1), BlockRoot: root1, AttIndex: uint16(vIdx1)},
		Att2: indexer.Locator{Slot: phase0.Slot(vSlot2), BlockRoot: root2, AttIndex: uint16(vIdx2)},
	}

	log.WithFields(logrus.Fields{
		"op1": fmt.Sprintf("slot %d root %s idx %d", vSlot1, vRoot1, vIdx1),
		"op2": fmt.Sprintf("slot %d root %s idx %d", vSlot2, vRoot2, vIdx2),
	}).Info("building attester slashing evidence")

	ev, err := evidence.BuildAttester(ctx, cand, headEpoch)
	if err != nil {
		return fmt.Errorf("build evidence: %w", err)
	}
	if ev == nil {
		return fmt.Errorf("could not build evidence (block unavailable, or the pair is not slashable on re-verification)")
	}

	log.WithFields(logrus.Fields{
		"submittable": ev.Submittable,
		"reason":      ev.Reason,
		"offenders":   ev.Offenders,
		"version":     ev.Slashing.Version.String(),
	}).Info("evidence built")

	body, _ := ev.Slashing.MarshalJSON()
	var pretty any
	_ = json.Unmarshal(body, &pretty)
	out, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Printf("\nAttesterSlashing (version %s):\n%s\n\n", ev.Slashing.Version.String(), out)

	if !vDoSubmit {
		log.Info("not submitting (pass --submit to broadcast to the network)")
		return nil
	}
	if !ev.Submittable {
		return fmt.Errorf("evidence is not submittable: %s", ev.Reason)
	}

	log.WithField("offenders", ev.Offenders).Warn("submitting attester slashing to the network")
	if err := beaconSvc.SubmitAttesterSlashing(ctx, ev.Slashing); err != nil {
		return fmt.Errorf("submit: %w", err)
	}
	log.Info("submitted successfully; the slashing is now in the beacon node's operation pool")

	// Give the network a few slots to include it, then report.
	secondsPerSlot := spec.SecondsPerSlot()
	if secondsPerSlot == 0 {
		secondsPerSlot = 12
	}
	log.WithField("offenders", ev.Offenders).Infof(
		"waiting ~%d slots for on-chain inclusion; verify with: dora /api/v1/validators/status?indices=%v",
		4, ev.Offenders)
	time.Sleep(time.Duration(secondsPerSlot*4) * time.Second)
	log.Info("done; check the offender validators' slashed status on chain")
	return nil
}
