package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/forkupdate"
	"github.com/spf13/cobra"
)

// These commands prepare and inspect local operation receipts. They deliberately
// do not wire a daemon-owned install path before lifecycle adapters are accepted.
func init() {
	group := &cobra.Command{Use: "fork-update", Short: "Verify Fork candidates and inspect local upgrade plans"}
	verify := &cobra.Command{Use: "verify", Short: "Verify a signed catalog against a pinned local policy", Args: cobra.NoArgs}
	var envelopePath, keyPath, channel, origin string
	verify.Flags().StringVar(&envelopePath, "catalog", "", "Signed catalog file")
	verify.Flags().StringVar(&keyPath, "public-key", "", "Pinned Ed25519 public key file (base64)")
	verify.Flags().StringVar(&channel, "channel", "", "Exact Fork channel")
	verify.Flags().StringVar(&origin, "origin", "", "Pinned HTTPS artifact origin")
	for _, name := range []string{"catalog", "public-key", "channel", "origin"} {
		_ = verify.MarkFlagRequired(name)
	}
	verify.RunE = func(cmd *cobra.Command, _ []string) error {
		candidate, err := readForkCatalog(envelopePath, keyPath, channel, origin)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(candidate)
	}
	plan := &cobra.Command{Use: "plan", Short: "Create an unapproved immutable upgrade plan", Args: cobra.NoArgs}
	var planCatalog, planKey, planChannel, planOrigin, inventory, receipt string
	plan.Flags().StringVar(&planCatalog, "catalog", "", "Signed catalog file")
	plan.Flags().StringVar(&planKey, "public-key", "", "Pinned public key file")
	plan.Flags().StringVar(&planChannel, "channel", "", "Exact Fork channel")
	plan.Flags().StringVar(&planOrigin, "origin", "", "Pinned HTTPS artifact origin")
	plan.Flags().StringVar(&inventory, "inventory", "", "Installed inventory JSON file")
	plan.Flags().StringVar(&receipt, "receipt", "", "New persistent technical receipt path")
	for _, name := range []string{"catalog", "public-key", "channel", "origin", "inventory", "receipt"} {
		_ = plan.MarkFlagRequired(name)
	}
	plan.RunE = func(cmd *cobra.Command, _ []string) error {
		candidate, err := readForkCatalog(planCatalog, planKey, planChannel, planOrigin)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(inventory)
		if err != nil {
			return err
		}
		var installed map[string]forkupdate.Baseline
		if err := json.Unmarshal(data, &installed); err != nil {
			return err
		}
		r, err := forkupdate.Plan(candidate, installed)
		if err != nil {
			return err
		}
		if err := (forkupdate.Journal{Path: receipt}).Create(r); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
	}
	status := &cobra.Command{Use: "status", Short: "Read a persistent upgrade receipt", Args: cobra.NoArgs}
	var statusPath string
	status.Flags().StringVar(&statusPath, "receipt", "", "Persistent technical receipt path")
	_ = status.MarkFlagRequired("receipt")
	status.RunE = func(cmd *cobra.Command, _ []string) error {
		r, err := (forkupdate.Journal{Path: statusPath}).Read()
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			forkupdate.Receipt
			Complete bool `json:"complete"`
		}{r, r.Complete()})
	}
	worker := &cobra.Command{Use: "worker", Short: "Reconcile one update tick outside the daemon/app lifecycle", Args: cobra.NoArgs}
	var configPath string
	worker.Flags().StringVar(&configPath, "config", "", "Private host update policy")
	_ = worker.MarkFlagRequired("config")
	worker.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := requireHumanLocalCommand("fork-update worker"); err != nil {
			return err
		}
		config, err := forkupdate.LoadWorker(configPath)
		if err != nil {
			return err
		}
		if config.TokenFile == "" {
			if strings.TrimRight(resolveServerURL(cmd), "/") != strings.TrimRight(config.ControlURL, "/") {
				return fmt.Errorf("update control URL differs from the native CLI profile")
			}
			config.NativeToken = resolveToken(cmd)
		}
		return config.Tick(cmd.Context())
	}
	publish := &cobra.Command{Use: "publish", Short: "Sign a complete private candidate bound to clean source and full gate evidence", Args: cobra.NoArgs}
	var publishCatalog, source, gateLog, packages, privateKey, publishOutput, publishOrigin string
	var isolation bool
	for name, pointer := range map[string]*string{"catalog": &publishCatalog, "source": &source, "gate-log": &gateLog, "packages": &packages, "private-key": &privateKey, "output": &publishOutput, "origin": &publishOrigin} {
		publish.Flags().StringVar(pointer, name, "", "Required private publication input")
		_ = publish.MarkFlagRequired(name)
	}
	publish.Flags().BoolVar(&isolation, "isolation", false, "Only publish unmerged isolation packages to selfhost-test")
	publish.RunE = func(cmd *cobra.Command, _ []string) error {
		data, err := os.ReadFile(publishCatalog)
		if err != nil {
			return err
		}
		var catalog forkupdate.Catalog
		if err = json.Unmarshal(data, &catalog); err != nil {
			return err
		}
		return forkupdate.Publish(cmd.Context(), source, gateLog, packages, privateKey, publishOutput, publishOrigin, catalog, isolation)
	}
	keygen := &cobra.Command{Use: "keygen", Short: "Create a new private local channel key directory", Args: cobra.NoArgs}
	var keyDir string
	keygen.Flags().StringVar(&keyDir, "dir", "", "New absolute private key directory")
	_ = keygen.MarkFlagRequired("dir")
	keygen.RunE = func(cmd *cobra.Command, _ []string) error {
		if !filepath.IsAbs(keyDir) {
			return fmt.Errorf("absolute new key directory required")
		}
		if _, err := os.Lstat(keyDir); !os.IsNotExist(err) {
			return fmt.Errorf("key directory exists or cannot be inspected")
		}
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(keyDir), 0700); err != nil {
			return err
		}
		staging, err := os.MkdirTemp(filepath.Dir(keyDir), ".channel-key-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		if err = os.WriteFile(filepath.Join(staging, "private-key.b64"), []byte(base64.StdEncoding.EncodeToString(private)), 0600); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(staging, "public-key.b64"), []byte(base64.StdEncoding.EncodeToString(public)), 0644); err != nil {
			return err
		}
		if err = os.Rename(staging, keyDir); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"public_key": base64.StdEncoding.EncodeToString(public), "directory": keyDir})
	}
	group.AddCommand(verify, plan, status, worker, publish, keygen)
	rootCmd.AddCommand(group)
}

func readForkCatalog(path, keyPath, channel, origin string) (forkupdate.VerifiedCatalog, error) {
	var empty forkupdate.VerifiedCatalog
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return empty, err
	}
	key, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(keyData)))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return empty, fmt.Errorf("invalid pinned public key")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty, err
	}
	return forkupdate.VerifyCatalog(data, forkupdate.Policy{PublicKey: key, Channel: channel, Origin: origin})
}
