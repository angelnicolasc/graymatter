package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	bolt "go.etcd.io/bbolt"
)

var (
	errDoctorNoDatabase  = errors.New("gray.db does not exist")
	errDoctorUninspected = errors.New("gray.db is a symbolic link and was not inspected")
	errDoctorIncomplete  = errors.New("daemon diagnostics incomplete")
)

// doctorReadOnlyVectors prevents memory.Open from constructing a persistent
// chromem backend, even if a vectors directory already exists.
type doctorReadOnlyVectors struct{}

func (doctorReadOnlyVectors) EnsureCollection(string) error { return nil }
func (doctorReadOnlyVectors) AddDocument(context.Context, string, string, string, []float32, map[string]string) error {
	return memory.ErrStoreReadOnly
}
func (doctorReadOnlyVectors) Query(context.Context, string, []float32, int) ([]memory.VectorResult, error) {
	return nil, errors.New("vector queries are unavailable in read-only setup diagnostics")
}
func (doctorReadOnlyVectors) Close() error { return nil }

type doctorStoreReader interface {
	ListAgents() ([]string, error)
	Stats(string) (memory.MemoryStats, error)
	DB() *bolt.DB
	Close() error
}

type doctorDaemonReader interface {
	ListAgents() ([]string, error)
	Stats(string) (memory.MemoryStats, error)
	PendingVectorCount() (int, error)
	StoreOverview() (*daemon.StoreOverviewResponse, error)
	KGState() (*daemon.KGStateResponse, error)
	Close() error
}

func openDiagnosticMemory(dir string) (*memory.Store, error) {
	return memory.Open(memory.StoreConfig{
		DataDir: dir, ReadOnly: true, VectorBackend: doctorReadOnlyVectors{},
	})
}

var doctorOpenReadOnlyStore = func(dir string) (doctorStoreReader, error) {
	return openDiagnosticMemory(dir)
}

var doctorConnectNoSpawn = func(dir string) (doctorDaemonReader, error) {
	return daemon.ConnectNoSpawn(dir)
}

var doctorDBLstat = os.Lstat

// doctorDBLeaf inspects the known DB leaf before any open or daemon request.
// A directory alias for dataDir remains valid; a leaf link is deliberately not
// followed. This is a read-only observation, not a TOCTOU confinement claim.
func doctorDBLeaf(dir string) error {
	path := filepath.Join(dir, "gray.db")
	info, err := doctorDBLstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return errDoctorNoDatabase
	}
	if err != nil {
		return fmt.Errorf("inspect gray.db: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errDoctorUninspected
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("gray.db is not a regular file (%s)", info.Mode())
	}
	return nil
}

func doctorDBError(c checkResult, err error) checkResult {
	switch {
	case errors.Is(err, errDoctorNoDatabase):
		c.Status, c.Detail = "info", "no database yet (gray.db is created on first write)"
		c.Hint = zeroFactsRestartHint
	case errors.Is(err, errDoctorUninspected):
		c.Status, c.Detail = "warn", "gray.db is a symbolic link; database not inspected"
		c.Hint = "select a data directory with a regular gray.db; doctor will not follow a linked DB leaf"
	default:
		c.Status, c.Detail = "fail", fmt.Sprintf("cannot inspect gray.db: %v", err)
	}
	return c
}

func checkStoreReadOnly(dir string) checkResult {
	c := checkResult{Name: "store"}
	if err := doctorDBLeaf(dir); err != nil {
		c = doctorDBError(c, err)
		if errors.Is(err, errDoctorNoDatabase) {
			return flagIfUnused(c, dir, 0)
		}
		return c
	}

	if dc, err := doctorConnectNoSpawn(dir); err == nil {
		return doctorDaemonStore(dc, dir)
	}
	return doctorDirectStore(dir)
}

func doctorDaemonStore(dc doctorDaemonReader, dir string) (c checkResult) {
	c = checkResult{Name: "store"}
	defer func() {
		if err := dc.Close(); err != nil {
			c.Status = "warn"
			if strings.Contains(c.Detail, "incomplete") {
				c.Detail += fmt.Sprintf("; connection close failed: %v", err)
			} else {
				c.Detail = fmt.Sprintf("daemon diagnostics incomplete: connection close failed: %v", err)
			}
			c.Hint = "run doctor again; counts cannot be certified from an incomplete diagnostic"
		}
	}()
	agents, err := dc.ListAgents()
	if err != nil {
		return doctorIncompleteStore(fmt.Errorf("list agents: %w", err))
	}
	facts := 0
	evidenceFacts, evidenceKnown := 0, true
	for _, a := range agents {
		st, err := dc.Stats(a)
		if err != nil {
			return doctorIncompleteStore(fmt.Errorf("stats for %q: %w", a, err))
		}
		facts += st.FactCount
		if st.LiveEvidenceCount == nil {
			evidenceKnown = false
		} else if *st.LiveEvidenceCount < 0 || *st.LiveEvidenceCount > st.FactCount {
			return doctorIncompleteStore(fmt.Errorf("invalid literal evidence count for %q", a))
		} else {
			evidenceFacts += *st.LiveEvidenceCount
		}
	}
	pending, err := dc.PendingVectorCount()
	if err != nil {
		return doctorIncompleteStore(fmt.Errorf("pending vectors: %w", err))
	}
	overview, err := dc.StoreOverview()
	if err != nil {
		return doctorIncompleteStore(fmt.Errorf("store overview: %w", err))
	}
	if overview == nil {
		return doctorIncompleteStore(errors.New("store overview returned no result"))
	}
	c.Status = "ok"
	c.Detail = fmt.Sprintf("served by daemon — observed %d fact(s) across %d agent(s) · historical generated cycles %d (facts consumed %d) · %s",
		facts, len(agents), overview.Consolidations, overview.FactsConsumed, maintenanceModeLabel(overview.MaintenanceMode))
	c.Detail += " · review journal " + reviewOverviewText(overview.Review)
	c.Detail += doctorCitationSummary(evidenceFacts, evidenceKnown)
	if pending > 0 {
		c.Status = "warn"
		c.Detail += fmt.Sprintf(", %d pending vector write(s)", pending)
		c.Hint = "pending vectors in a quiescent system mean the embedding backend is failing — check your embedding configuration (Ollama URL / API keys)"
	}
	addDoctorSubjectIssues(&c, dc)
	return flagIfUnused(c, dir, facts)
}

func doctorIncompleteStore(err error) checkResult {
	return checkResult{
		Name: "store", Status: "warn",
		Detail: fmt.Sprintf("daemon diagnostics incomplete: %v (fact total not reported)", err),
		Hint:   "run doctor again; partial RPC results are not an exact store total",
	}
}

func doctorDirectStore(dir string) (c checkResult) {
	c = checkResult{Name: "store"}
	store, err := doctorOpenReadOnlyStore(dir)
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			c.Status, c.Detail = "warn", "gray.db is held by a non-daemon process (bbolt is single-writer)"
			c.Hint = "another program is holding the store directly; close it and retry" + lsofHint(filepath.Join(dir, "gray.db"))
			return c
		}
		c.Status, c.Detail = "fail", fmt.Sprintf("store failed to open read-only: %v", err)
		return c
	}
	defer func() {
		if err := store.Close(); err != nil {
			c.Status = "fail"
			if strings.Contains(c.Detail, "failed") {
				c.Detail += fmt.Sprintf("; read-only store close failed: %v", err)
			} else {
				c.Detail = fmt.Sprintf("read-only store close failed: %v", err)
			}
			c.Hint = "store diagnostics could not be completed"
		}
	}()
	agents, err := store.ListAgents()
	if err != nil {
		c.Status, c.Detail = "fail", fmt.Sprintf("store opened but listing agents failed: %v", err)
		return c
	}
	facts := 0
	evidenceFacts, evidenceKnown := 0, true
	perAgent := make(map[string]int, len(agents))
	for _, a := range agents {
		st, err := store.Stats(a)
		if err != nil {
			c.Status, c.Detail = "fail", fmt.Sprintf("store stats for %q failed: %v", a, err)
			return c
		}
		facts += st.FactCount
		perAgent[a] = st.FactCount
		if st.LiveEvidenceCount == nil {
			evidenceKnown = false
		} else if *st.LiveEvidenceCount < 0 || *st.LiveEvidenceCount > st.FactCount {
			c.Status, c.Detail = "fail", fmt.Sprintf("invalid literal evidence count for %q", a)
			return c
		} else {
			evidenceFacts += *st.LiveEvidenceCount
		}
	}
	if err := doctorValidateFactCounts(store.DB(), perAgent); err != nil {
		c.Status, c.Detail = "fail", fmt.Sprintf("store facts could not be counted reliably: %v", err)
		return c
	}
	pending, cycles, consumed, err := doctorReadCounters(store.DB())
	if err != nil {
		c.Status, c.Detail = "fail", fmt.Sprintf("store counters failed: %v", err)
		return c
	}
	c.Status = "ok"
	c.Detail = fmt.Sprintf("no daemon running — %d fact(s) across %d agent(s) (direct read) · historical generated cycles %d (facts consumed %d)", facts, len(agents), cycles, consumed)
	c.Detail += doctorCitationSummary(evidenceFacts, evidenceKnown)
	review, err := memory.ReadReviewOverview(context.Background(), store.DB())
	if err != nil {
		c.Status = "warn"
		c.Detail += fmt.Sprintf(" · review journal unavailable (%v)", err)
	} else {
		c.Detail += " · review journal " + reviewOverviewText(&review)
	}
	if pending > 0 {
		c.Status = "warn"
		c.Detail += fmt.Sprintf(", %d pending vector write(s)", pending)
		c.Hint = "pending vectors in a quiescent system mean the embedding backend is failing — check your embedding configuration (Ollama URL / API keys)"
	}
	addDoctorSubjectIssues(&c, store)
	return flagIfUnused(c, dir, facts)
}

func addDoctorSubjectIssues(check *checkResult, store any) {
	reader, ok := store.(interface {
		SubjectIssuesByNamespace(context.Context) (map[string]memory.SubjectIssues, error)
	})
	if !ok {
		return
	}
	byNamespace, err := reader.SubjectIssuesByNamespace(context.Background())
	if err != nil {
		check.Status = "warn"
		check.Detail += fmt.Sprintf(" · subject diagnostics unavailable (%q)", err.Error())
		return
	}
	var namespaces []string
	for namespace := range byNamespace {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	var issues memory.SubjectIssues
	var projects []string
	for _, namespace := range namespaces {
		report := byNamespace[namespace]
		if report.Total == 0 {
			continue
		}
		issues.Total += report.Total
		issues.Subjects = append(issues.Subjects, report.Subjects[:min(len(report.Subjects), 3-len(issues.Subjects))]...)
		projects = append(projects, fmt.Sprintf("%q (%d)", namespace, report.Total))
	}
	if issues.Total == 0 {
		return
	}
	check.Status = "warn"
	check.Detail += " · " + issues.Warning() + " Projects: " + strings.Join(projects, ", ")
	if check.Hint != "" {
		check.Hint += " "
	}
	check.Hint += "Use core memory_search for the subject diagnostic. Recover with memory_add, the same subject, and an explicit replaces ID. See docs/subject-memory.md."
}

func doctorCitationSummary(count int, known bool) string {
	if !known {
		return " · literal evidence count unavailable from this store"
	}
	return fmt.Sprintf(" · %d live fact(s) with literal evidence", count)
}

// Store.Stats uses List, which intentionally skips malformed fact records.
// Doctor must detect that loss rather than publish a plausible but low total.
func doctorValidateFactCounts(db *bolt.DB, decoded map[string]int) error {
	if db == nil {
		return errors.New("store has no database handle")
	}
	return db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket([]byte("facts"))
		if root == nil {
			for agent, n := range decoded {
				if n != 0 {
					return fmt.Errorf("agent %q has %d decoded facts but no facts bucket", agent, n)
				}
			}
			return nil
		}
		seen := make(map[string]bool, len(decoded))
		if err := root.ForEach(func(k, v []byte) error {
			if v != nil {
				return fmt.Errorf("unexpected value in facts root")
			}
			agent := string(k)
			sub := root.Bucket(k)
			if sub == nil {
				return fmt.Errorf("missing fact bucket for %q", agent)
			}
			count := 0
			if err := sub.ForEach(func(_, value []byte) error {
				if value == nil {
					return fmt.Errorf("nested bucket in facts for %q", agent)
				}
				count++
				return nil
			}); err != nil {
				return err
			}
			if count != decoded[agent] {
				return fmt.Errorf("agent %q has %d raw facts but %d decoded facts", agent, count, decoded[agent])
			}
			seen[agent] = true
			return nil
		}); err != nil {
			return err
		}
		for agent, n := range decoded {
			if !seen[agent] && n != 0 {
				return fmt.Errorf("agent %q has %d decoded facts but no fact bucket", agent, n)
			}
		}
		return nil
	})
}

func doctorReadCounters(db *bolt.DB) (pending, cycles, consumed int, err error) {
	if db == nil {
		return 0, 0, 0, errors.New("store has no database handle")
	}
	err = db.View(func(tx *bolt.Tx) error {
		if meta := tx.Bucket([]byte("meta")); meta != nil {
			for _, item := range []struct {
				key string
				dst *int
			}{{"consolidations", &cycles}, {"facts_consolidated", &consumed}} {
				if raw := meta.Get([]byte(item.key)); raw != nil {
					n, parseErr := strconv.Atoi(string(raw))
					if parseErr != nil || n < 0 {
						return fmt.Errorf("invalid %s counter", item.key)
					}
					*item.dst = n
				}
			}
		}
		if root := tx.Bucket([]byte("pending_vector")); root != nil {
			return root.ForEach(func(k, v []byte) error {
				if v != nil {
					return nil
				}
				sub := root.Bucket(k)
				if sub == nil {
					return fmt.Errorf("pending vector bucket %q is missing", k)
				}
				pending += sub.Stats().KeyN
				return nil
			})
		}
		return nil
	})
	return pending, cycles, consumed, err
}

// doctorGraphState preserves the distinction between absent graph buckets,
// failed observations and a valid graph with zero nodes.
func doctorGraphState(dir string) (state daemon.KGStateResponse, err error) {
	if err := doctorDBLeaf(dir); err != nil {
		return state, err
	}
	if dc, err := doctorConnectNoSpawn(dir); err == nil {
		state, callErr := dc.KGState()
		closeErr := dc.Close()
		if callErr != nil {
			return daemon.KGStateResponse{}, fmt.Errorf("%w: graph state: %v", errDoctorIncomplete, callErr)
		}
		if closeErr != nil {
			return daemon.KGStateResponse{}, fmt.Errorf("%w: close connection: %v", errDoctorIncomplete, closeErr)
		}
		if state == nil {
			return daemon.KGStateResponse{}, fmt.Errorf("%w: graph state returned no result", errDoctorIncomplete)
		}
		return *state, nil
	}
	store, err := doctorOpenReadOnlyStore(dir)
	if err != nil {
		return state, err
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close read-only graph store: %w", closeErr))
		}
	}()
	if store.DB() == nil {
		return state, errors.New("store has no database handle")
	}
	graph, err := kg.OpenRead(store.DB())
	if errors.Is(err, kg.ErrNoGraph) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	inspection, err := daemon.ReadKGInspection(graph)
	if err != nil {
		return state, err
	}
	state.Nodes = len(inspection.Nodes)
	state.Edges = len(inspection.Edges)
	state.Provenance = inspection.Provenance
	return state, nil
}
