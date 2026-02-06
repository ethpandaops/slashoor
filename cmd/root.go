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

	"github.com/slashoor/slashoor/pkg/coordinator"
)

var (
	cfgFile   string
	logLevel  string
	startSlot uint64
)

var rootCmd = &cobra.Command{
	Use:   "slashoor",
	Short: "Ethereum beacon chain attester slashing detector",
	Long: `Slashoor is a lazy slasher implementation that detects and submits
attester slashings on the Ethereum beacon chain using the m(i)/M(i) algorithm.`,
	RunE: run,
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "config.yaml", "config file path")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().Uint64Var(&startSlot, "start-slot", 0, "rescan from this slot to head on startup (0 = disabled)")
	rootCmd.AddCommand(versionCmd)
}

func run(cmd *cobra.Command, args []string) error {
	log := logrus.New()

	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		return fmt.Errorf("invalid log level: %w", err)
	}

	log.SetLevel(level)
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	cfg, err := loadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cmd.Flags().Changed("start-slot") {
		cfg.StartSlot = startSlot
		cfg.StartSlotEnabled = true
		log.WithField("start_slot", startSlot).Info("rescanning from specified slot")
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

	log.Info("slashoor started successfully")

	<-sigCh
	log.Info("shutdown signal received")

	if err := coord.Stop(); err != nil {
		log.WithError(err).Error("error during shutdown")
	}

	log.Info("slashoor stopped")

	return nil
}

func loadConfig(path string) (*coordinator.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg coordinator.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &cfg, nil
}
