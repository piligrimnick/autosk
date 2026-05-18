// Package agent owns the `agents` table.
//
// In v0.2 (see docs/plans/20260517-Workflows-Plan.md §2.1) an agent is
// a minimal DB row: id + name + is_human + created_at. The executable
// configuration (model, thinking, first-turn prompt, extra args) is no
// longer stored next to the project — it lives in npm packages
// installed into the autosk packages prefix (see
// internal/agent/pkgregistry and docs/plans/20260518-Agent-Packages.md).
//
// `human` is seeded by migrations.SeedHumanAgent on first migrate.
// Other agents are created either explicitly (`autosk agent install
// @scope/name`) or lazily when $AUTOSK_AGENT names one that doesn't
// exist yet. The lazy path is gated by an optional PackageResolver
// (see WithResolver / EnsureByName) so the CLI fails fast when a
// workflow references a package that isn't installed.
package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/id"
)

// IDPrefix is the prefix for agent ids ("ag-XXXX").
const IDPrefix = "ag"

// Sentinel errors.
var (
	ErrNotOpen         = errors.New("agent store: not open")
	ErrNotFound        = errors.New("agent not found")
	ErrAlreadyExist    = errors.New("agent already exists")
	ErrInvalidName     = errors.New("invalid agent name")
	ErrNotInstalled    = errors.New("agent_not_installed")
)

// PackageResolver is the validation hook used by EnsureByName and
// Create to confirm that a non-human agent name corresponds to an
// installed agent package. The pkgregistry.Registry satisfies it.
type PackageResolver interface {
	Has(name string) bool
}

// Agent is the in-memory representation of an `agents` row.
type Agent struct {
	ID        string
	Name      string
	IsHuman   bool
	CreatedAt time.Time
}

// Store wraps the `agents` table CRUD on the shared *sql.DB.
type Store struct {
	db       *sql.DB
	resolver PackageResolver
}

// New constructs a Store. Migrations must already have run.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// WithResolver attaches a PackageResolver so that Create/EnsureByName
// reject names other than "human" that are not registered. Passing nil
// detaches the resolver (the default — useful for tests that don't
// touch the global packages prefix).
func (s *Store) WithResolver(r PackageResolver) *Store {
	s.resolver = r
	return s
}

// HumanAgentName is the reserved name of the seeded human agent. The
// resolver is bypassed for this name.
const HumanAgentName = "human"

// Create inserts a new agent. Returns ErrAlreadyExist on UNIQUE(name).
// Name is normalised (trimmed). is_human=1 only when name is literally
// "human"; the CLI's `--human` flag adds it for any name and is gated by
// the caller, not here.
func (s *Store) Create(ctx context.Context, name string, isHuman bool) (Agent, error) {
	if s.db == nil {
		return Agent{}, ErrNotOpen
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Agent{}, fmt.Errorf("%w: empty", ErrInvalidName)
	}
	if strings.ContainsAny(name, " \t\n\r") {
		return Agent{}, fmt.Errorf("%w: contains whitespace", ErrInvalidName)
	}
	if s.resolver != nil && name != HumanAgentName && !s.resolver.Has(name) {
		return Agent{}, fmt.Errorf("%w: %s (run: autosk agent install %s)", ErrNotInstalled, name, name)
	}
	agentID, err := id.NewUnique(IDPrefix, func(candidate string) (bool, error) {
		return s.idExists(ctx, candidate)
	})
	if err != nil {
		return Agent{}, fmt.Errorf("generate id: %w", err)
	}
	human := 0
	if isHuman {
		human = 1
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO agents(id, name, is_human, created_at) VALUES (?, ?, ?, ?)`,
		agentID, name, human, now)
	if err != nil {
		if isUniqueErr(err) {
			return Agent{}, fmt.Errorf("%w: %s", ErrAlreadyExist, name)
		}
		return Agent{}, fmt.Errorf("insert agent: %w", err)
	}
	return Agent{ID: agentID, Name: name, IsHuman: isHuman, CreatedAt: time.Unix(now, 0).UTC()}, nil
}

// GetByName returns the agent with the given name. ErrNotFound if absent.
func (s *Store) GetByName(ctx context.Context, name string) (Agent, error) {
	if s.db == nil {
		return Agent{}, ErrNotOpen
	}
	return s.scanOne(ctx, `WHERE name = ?`, name)
}

// GetByID returns the agent with the given id. ErrNotFound if absent.
func (s *Store) GetByID(ctx context.Context, agentID string) (Agent, error) {
	if s.db == nil {
		return Agent{}, ErrNotOpen
	}
	return s.scanOne(ctx, `WHERE id = ?`, agentID)
}

// List returns all agents sorted by name.
func (s *Store) List(ctx context.Context) ([]Agent, error) {
	if s.db == nil {
		return nil, ErrNotOpen
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, is_human, created_at FROM agents ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// EnsureByName returns the agent with the given name, creating it if
// it doesn't exist. Used by the lazy-resolve path that maps
// $AUTOSK_AGENT to an agent row on the fly. is_human is auto-set to
// true for the literal name "human", false otherwise (callers wanting
// an explicit non-default human flag should use Create directly).
//
// When a PackageResolver is attached (WithResolver), names other than
// "human" must be present in the resolver, otherwise the call fails
// with ErrNotInstalled before any DB write.
func (s *Store) EnsureByName(ctx context.Context, name string) (Agent, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Agent{}, fmt.Errorf("%w: empty", ErrInvalidName)
	}
	a, err := s.GetByName(ctx, name)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Agent{}, err
	}
	isHuman := name == HumanAgentName
	return s.Create(ctx, name, isHuman)
}

// ---- internals ------------------------------------------------------------

func (s *Store) idExists(ctx context.Context, agentID string) (bool, error) {
	var x int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM agents WHERE id = ?`, agentID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) scanOne(ctx context.Context, suffix string, args ...any) (Agent, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, is_human, created_at FROM agents `+suffix, args...)
	a, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRow(sc scanner) (Agent, error) {
	var (
		a         Agent
		human     int
		createdAt int64
	)
	if err := sc.Scan(&a.ID, &a.Name, &human, &createdAt); err != nil {
		return Agent{}, err
	}
	a.IsHuman = human != 0
	a.CreatedAt = time.Unix(createdAt, 0).UTC()
	return a, nil
}

// isUniqueErr returns true when err is the SQLite UNIQUE constraint violation
// on the agents(name) index. We match by substring to avoid pulling in
// mattn/go-sqlite3's typed errors at this layer.
func isUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed: agents.name")
}
