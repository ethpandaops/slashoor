package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/slashoor/pkg/coordinator"
)

var (
	cfgFile     string
	logLevel    string
	resetToSlot int64
)

var rootCmd = &cobra.Command{
	Use:   "slashoor",
	Short: "Ethereum beacon chain slasher",
	Long: `Slashoor detects and submits attester slashings (double and surround
votes) and proposer slashings (double proposals) on the Ethereum beacon chain,
for both live and historic offenses. It uses Dora as a durable block/duty store
and beacon nodes for live data and submission.`,
	RunE: run,
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "config.yaml", "config file path")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().Int64Var(&resetToSlot, "reset-to-slot", -1,
		"wipe the index and re-scan from this slot (0 = genesis); required when moving the start earlier")
	rootCmd.AddCommand(versionCmd)
}

func run(cmd *cobra.Command, _ []string) error {
	log := logrus.New()
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		return fmt.Errorf("invalid log level: %w", err)
	}
	log.SetLevel(level)
	log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	cfg, err := loadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cmd.Flags().Changed("reset-to-slot") {
		cfg.ResetEnabled = true
		cfg.ResetToSlot = resetToSlot
		log.WithField("reset_to_slot", resetToSlot).Warn("index will be reset")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	coord, err := coordinator.New(cfg, log)
	if err != nil {
		return fmt.Errorf("failed to create coordinator: %w", err)
	}
	if err := coord.Start(ctx); err != nil {
		return fmt.Errorf("failed to start coordinator: %w", err)
	}
	log.Info("slashoor started")

	<-sigCh
	log.Info("shutdown signal received")
	if err := coord.Stop(); err != nil {
		log.WithError(err).Error("error during shutdown")
	}
	return nil
}

func loadConfig(path string) (*coordinator.Config, error) {
	cfg := coordinator.DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	return cfg, nil
}
