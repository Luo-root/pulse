package systemtest

// Minimal helpers adapted from distributed/systemtest/{harness.go,process_test.go}
// at pulse-durable b2e69374f8653c3cb2eb073d5d293ad05d043544. Unrelated harness,
// benchmark, ingress and MQ functionality is intentionally excluded.
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	d "github.com/Luo-root/pulse-durable/distributed"
	"github.com/Luo-root/pulse-durable/distributed/postgres"
	"github.com/Luo-root/pulse-durable/distributed/worker"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

type Config struct {
	Namespace, ID string
	Capacity      int
}

func (c Config) workerConfig(backend d.Coordinator, renew worker.Renewer, reg *d.Registry, observe func(worker.Event)) worker.Config {
	return worker.Config{Coordinator: backend, Renewer: renew, Registry: reg, Namespace: c.Namespace, WorkerID: c.ID, Capacity: c.Capacity, BatchMax: c.Capacity, TTL: 2 * time.Second, RenewInterval: 300 * time.Millisecond, RenewTimeout: 400 * time.Millisecond, OperationTimeout: time.Second, PollMin: 20 * time.Millisecond, PollMax: 50 * time.Millisecond, Observe: observe}
}

type Event struct {
	Kind   string   `json:"kind"`
	PID    int      `json:"pid"`
	Worker d.Worker `json:"worker"`
	RunID  string   `json:"run_id,omitempty"`
	Epoch  int64    `json:"epoch,omitempty"`
	Reason string   `json:"reason,omitempty"`
}
type emitter struct {
	mu       sync.Mutex
	out      io.Writer
	identity d.Worker
}

func (e *emitter) emit(v Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v.PID = os.Getpid()
	v.Worker = e.identity
	_ = json.NewEncoder(e.out).Encode(v)
}

type observed struct {
	d.Coordinator
	emit   *emitter
	mu     sync.Mutex
	tokens map[string]d.Token
}

func (o *observed) Claim(ctx context.Context, r d.ClaimRequest) ([]d.Claim, error) {
	rows, err := o.Coordinator.Claim(ctx, r)
	if err == nil {
		o.mu.Lock()
		for _, c := range rows {
			o.tokens[c.Token.Identity.RunID] = c.Token
			o.emit.emit(Event{Kind: "owned", RunID: c.Token.Identity.RunID, Epoch: c.Token.Epoch})
		}
		o.mu.Unlock()
	}
	return rows, err
}
func (o *observed) token(id string) d.Token { o.mu.Lock(); defer o.mu.Unlock(); return o.tokens[id] }

type process struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	events []Event
	done   chan error
}

func (p *process) snapshot() []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Event(nil), p.events...)
}
func (p *process) has(kind string) bool {
	for _, e := range p.snapshot() {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
func await(t *testing.T, f func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

type fixture struct {
	c       *postgres.Coordinator
	admin   *pgxpool.Pool
	dsn, ns string
}

func provision(t *testing.T) fixture {
	t.Helper()
	dsn := os.Getenv("PULSE_DISTRIBUTED_TEST_DSN")
	if dsn == "" || os.Getenv("PULSE_DISTRIBUTED_TEST_ALLOW") != "yes" {
		t.Skip("PULSE_DISTRIBUTED_TEST_DSN and PULSE_DISTRIBUTED_TEST_ALLOW=yes required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	c := cfg.ConnConfig
	// Preserve the source harness's local disposable database allowlist. Never
	// accept a production database just because the opt-in flag is present.
	allowedDatabase := c.Database == "durable_distributed_test" || c.Database == "durable_dist_security_20261025"
	if c.Host != "127.0.0.1" || c.Port != 55432 || !allowedDatabase || c.User != "durable_test" {
		t.Fatal("provisioning restricted to authorized local test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("admin connection failed")
	}
	t.Cleanup(admin.Close)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	role := "pulse_sys_" + suffix
	ns := "systemtest-" + suffix
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal("isolated credential generation failed")
	}
	password := hex.EncodeToString(secret) // safe SQL hex literal, generated per fixture
	if _, err = admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD '"+password+"'"); err != nil {
		t.Fatal("isolated role creation failed")
	}
	// As in the source harness, retain evidence; never DROP shared schema/roles.
	if err = postgres.Migrate(ctx, dsn, postgres.Grant{Role: role, Namespace: ns, Permissions: []string{"query", "submit", "worker", "cancel"}}); err != nil {
		t.Fatal("isolated namespace migration failed")
	}
	u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))), Path: "/" + c.Database, User: url.UserPassword(role, password)}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	backend, err := postgres.New(ctx, u.String())
	if err != nil {
		t.Fatal("isolated backend unavailable")
	}
	t.Cleanup(backend.Close)
	return fixture{backend, admin, u.String(), ns}
}
func (f fixture) status(t *testing.T, id string) d.Snapshot {
	t.Helper()
	s, err := f.c.Get(context.Background(), d.Identity{Namespace: f.ns, RunID: id})
	if err != nil {
		t.Fatal("status query failed")
	}
	return s
}
func (f fixture) success(t *testing.T, id string) {
	t.Helper()
	await(t, func() bool { return f.status(t, id).Status == d.Succeeded }, "authoritative success "+id)
}
