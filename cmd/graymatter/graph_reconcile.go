package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	bolt "go.etcd.io/bbolt"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
)

type graphReconciler interface {
	KGReconcile(*daemon.KGReconcileRequest) error
}

func (r *reconnectingStore) KGReconcile(req *daemon.KGReconcileRequest) error {
	return r.mutate(func(s cliStore) error {
		reconciler, ok := s.(graphReconciler)
		if !ok {
			return errors.New("graph reconciliation is unavailable on this store")
		}
		return reconciler.KGReconcile(req)
	})
}

// reconcileGraph shares an existing daemon's writer or takes the lock on
// the existing database directly. A general store open can migrate the edge
// before the reconciliation transaction compares it with the inspected proof.
func reconcileGraph(dir string, req *daemon.KGReconcileRequest) error {
	if err := doctorDBLeaf(dir); err != nil {
		return err
	}
	useDaemon := !noDaemon && os.Getenv("GRAYMATTER_NO_DAEMON") != "1"
	if useDaemon {
		if handled, err := reconcileGraphOnDaemon(dir, req); handled {
			return err
		}
	}

	db, err := bolt.Open(filepath.Join(dir, "gray.db"), 0o600, &bolt.Options{
		Timeout:  2 * time.Second,
		OpenFile: openReconciliationFile,
	})
	if err != nil {
		// A daemon can win the lock after the first dial. A timeout here
		// proves that the direct mutation was never dispatched.
		if useDaemon && errors.Is(err, bolt.ErrTimeout) {
			if handled, err := reconcileGraphOnDaemon(dir, req); handled {
				return err
			}
		}
		return fmt.Errorf("open existing gray.db for reconciliation: %w", err)
	}
	defer func() { _ = db.Close() }()

	graph, err := kg.OpenRead(db)
	if err != nil {
		return err
	}
	return graph.ReconcileProvenance(req.Expected, req.ManualSupport, req.Supports)
}

func openReconciliationFile(path string, _ int, _ os.FileMode) (*os.File, error) {
	// bbolt's default opener includes O_CREATE. Reconciliation needs an
	// existing graph and cannot initialize a missing or empty database.
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Size() == 0) {
		err = errors.New("gray.db is not a nonempty regular file")
	}
	if err == nil {
		leaf, leafErr := os.Lstat(path)
		err = leafErr
		if err == nil && (!leaf.Mode().IsRegular() || !os.SameFile(info, leaf)) {
			err = errors.New("gray.db changed while opening it for reconciliation")
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func reconcileGraphOnDaemon(dir string, req *daemon.KGReconcileRequest) (bool, error) {
	connect := func() (cliStore, error) {
		client, err := daemon.ConnectNoSpawn(dir)
		if err != nil {
			return nil, err
		}
		return daemonStore{Client: client}, nil
	}
	initial, err := connect()
	if err != nil {
		return false, nil
	}
	// Reuse the mutation wrapper so a lost reply remains an unknown
	// outcome. Its health probe may reconnect to a running daemon only.
	store := newReconnectingStoreAt(initial, connect)
	defer func() { _ = store.Close() }()
	return true, store.KGReconcile(req)
}

func kgReconcileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile <evidence.json>",
		Short: "Reconcile one graph edge using explicit provenance evidence",
		Long: `Submit a proof for one graph edge. The evidence file contains the exact edge
snapshot under "expected", an explicit "manual_support" boolean, and "supports"
with the namespace, fact_id, kind and text_sha256 of each verified source.

Use kg inspect --json to retrieve the current edge snapshot. Supply complete
evidence only. The store rejects a changed edge, a changed fact revision, or
proof that omits recorded support. An existing daemon can handle the request.
When the database is idle, reconciliation takes its write lock directly.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := loadGraphReconciliation(args[0])
			if err != nil {
				return err
			}
			if err := reconcileGraph(dataDir, req); err != nil {
				return fmt.Errorf("reconcile graph edge: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Graph edge provenance reconciled.")
			return err
		},
	}
}

func loadGraphReconciliation(path string) (*daemon.KGReconcileRequest, error) {
	input, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open reconciliation evidence: %w", err)
	}
	defer func() { _ = input.Close() }()

	var evidence struct {
		Expected      kg.Edge `json:"expected"`
		ManualSupport *bool   `json:"manual_support"`
		Supports      []struct {
			Namespace  string  `json:"namespace"`
			FactID     string  `json:"fact_id"`
			Kind       *string `json:"kind"`
			TextSHA256 string  `json:"text_sha256"`
		} `json:"supports"`
	}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return nil, fmt.Errorf("decode reconciliation evidence: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("reconciliation evidence must contain exactly one JSON object")
	}
	if evidence.Expected.From == "" || evidence.Expected.To == "" || evidence.ManualSupport == nil || evidence.Supports == nil {
		return nil, errors.New("reconciliation evidence requires an expected edge, manual_support, and a supports array")
	}
	supports := make([]kg.SourceEvidence, 0, len(evidence.Supports))
	for _, source := range evidence.Supports {
		if source.Kind == nil {
			return nil, errors.New("each reconciliation source requires an explicit kind")
		}
		supports = append(supports, kg.SourceEvidence{
			Namespace: source.Namespace, FactID: source.FactID, Kind: *source.Kind, TextSHA256: source.TextSHA256,
		})
	}
	return &daemon.KGReconcileRequest{
		Expected: evidence.Expected, ManualSupport: *evidence.ManualSupport, Supports: supports,
	}, nil
}
