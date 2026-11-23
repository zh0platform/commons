// Package testdb gives tests their own PostgreSQL database on the server `go run ./test/withdb` started: a clone of
// a template that holds what infra provisions (the three logins, schema bff owned by the migration login).
package testdb

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"git.kloudbox.de/commons/bff/internal/db"
)

// The variables withdb hands to the test command. None starts with PG, which bff refuses.
const (
	HostVariable = "BFF_TEST_DB_HOST"
	PortVariable = "BFF_TEST_DB_PORT"
	CAVariable   = "BFF_TEST_DB_CA"
	// AdminVariable holds the administrator's password; LoginsVariable the three logins' password.
	AdminVariable  = "BFF_TEST_DB_ADMIN_PASSWORD"
	LoginsVariable = "BFF_TEST_DB_LOGIN_PASSWORD"
)

// The logins infra creates, and the template every test database is cloned from.
const (
	MigrateLogin   = "bff_migrate"
	RuntimeLogin   = "bff_runtime"
	LifecycleLogin = "bff_lifecycle"
	template       = "bff_template"
	admin          = "postgres"
	// PlaintextPrefix names the databases on which the server accepts bff's logins without TLS (lost-commit tests).
	PlaintextPrefix = "lostcommit_"
	// cloneLock serializes CREATE DATABASE … TEMPLATE, which fails while another copy of the template runs.
	cloneLock = 7_150_001
)

// Provision creates the logins and the template, as infra does for a product database.
func Provision(ctx context.Context, settings map[string]string) error {
	conn, err := connect(ctx, settings, admin, settings[AdminVariable], "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	// A failed statement is logged whole, and the server's log is printed when a run fails: the statements that carry
	// the password run under log_min_error_statement = panic, so a failed one is not logged.
	statements := []string{"BEGIN", "SET LOCAL log_min_error_statement = panic"}
	for _, login := range []string{MigrateLogin, RuntimeLogin, LifecycleLogin} {
		statements = append(statements, "CREATE ROLE "+login+" LOGIN PASSWORD '"+settings[LoginsVariable]+"'")
	}
	statements = append(statements, "COMMIT", "CREATE DATABASE "+template+" TEMPLATE template0")
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return err
		}
	}
	tmpl, err := connect(ctx, settings, admin, settings[AdminVariable], template)
	if err != nil {
		return err
	}
	defer func() { _ = tmpl.Close(ctx) }()
	_, err = tmpl.Exec(ctx, "CREATE SCHEMA bff AUTHORIZATION "+MigrateLogin)
	return err
}

// Database is one test's own database.
type Database struct {
	Name     string
	settings map[string]string
}

// New clones the template into a database only this test uses, closed like infra's (CONNECT for the three logins),
// and drops it when the test ends. A missing server is a setup failure, never a skip.
func New(t testing.TB) *Database {
	t.Helper()
	return newNamed(t, "test_")
}

// NewMigrated is New with the shipped migrations applied by the real runner.
func NewMigrated(t testing.TB) *Database {
	t.Helper()
	database := New(t)
	database.Migrate(t)
	return database
}

// Migrate applies the shipped migrations with the real runner.
func (database *Database) Migrate(t testing.TB) {
	t.Helper()
	pool, err := db.Open(database.Settings(MigrateLogin), db.Migrate)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Migrate(context.Background(), db.Shipped()); err != nil {
		t.Fatalf("migrate the test database: %v", err)
	}
}

// NewPlaintext is New for a database the server also accepts without TLS (the lost-commit tests' forwarder).
func NewPlaintext(t testing.TB) *Database {
	t.Helper()
	return newNamed(t, PlaintextPrefix)
}

func newNamed(t testing.TB, prefix string) *Database {
	t.Helper()
	settings := environment(t)
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	db := &Database{Name: prefix + hex.EncodeToString(raw), settings: settings}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := connect(ctx, settings, admin, settings[AdminVariable], "postgres")
	if err != nil {
		t.Fatalf("test database server: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	t.Cleanup(func() { db.drop(t) })
	statements := []string{
		"SELECT pg_advisory_lock(" + strconv.Itoa(cloneLock) + ")",
		"CREATE DATABASE " + db.Name + " TEMPLATE " + template,
		"SELECT pg_advisory_unlock(" + strconv.Itoa(cloneLock) + ")",
		"REVOKE ALL ON DATABASE " + db.Name + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + db.Name + " TO " + MigrateLogin + ", " + RuntimeLogin + ", " + LifecycleLogin,
	}
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("create test database: %v", err)
		}
	}
	return db
}

func (d *Database) drop(t testing.TB) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := connect(ctx, d.settings, admin, d.settings[AdminVariable], "postgres")
	if err != nil {
		t.Errorf("drop %s: %v", d.Name, err)
		return
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+d.Name+" WITH (FORCE)"); err != nil {
		t.Errorf("drop %s: %v", d.Name, err)
	}
}

// RestoreLikeInfra dumps this database and restores it the way infra's restore does: pg_dump in custom format, a
// target created closed from template0 (CONNECT for the migrate and lifecycle logins only, not yet the runtime
// login), pg_restore in one transaction with owners and grants. Both programs run inside the server's container.
func (d *Database) RestoreLikeInfra(t testing.TB) *Database {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := connect(ctx, d.settings, admin, d.settings[AdminVariable], "postgres")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	target := &Database{Name: "restored_" + d.Name, settings: d.settings}
	dump := "/tmp/" + d.Name + ".dump"
	t.Cleanup(func() { target.drop(t) })
	statements := []string{
		"COPY (SELECT 1) TO PROGRAM 'pg_dump --format=custom --file=" + dump + " --dbname=" + d.Name + "'",
		"CREATE DATABASE " + target.Name + " OWNER postgres TEMPLATE template0 ALLOW_CONNECTIONS false",
		"REVOKE ALL ON DATABASE " + target.Name + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + target.Name + " TO " + MigrateLogin + ", " + LifecycleLogin,
		"ALTER DATABASE " + target.Name + " ALLOW_CONNECTIONS true",
		"COPY (SELECT 1) TO PROGRAM 'pg_restore --single-transaction --exit-on-error --dbname=" + target.Name + " " + dump + "'",
	}
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("restore: %v", err)
		}
	}
	if _, err := conn.Exec(ctx, "COPY (SELECT 1) TO PROGRAM 'rm -f "+dump+"'"); err != nil {
		t.Fatalf("remove the dump: %v", err)
	}
	return target
}

// WaitForLockWait waits until at least waiters connections of the given application name wait on a lock here. Only
// the administrator sees another login's wait state.
func (d *Database) WaitForLockWait(t testing.TB, application string, waiters int, within time.Duration) {
	t.Helper()
	admin := d.Admin(t)
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		var waiting int
		if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = $1 AND application_name = $2 AND wait_event_type = 'Lock'`, d.Name, application).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= waiters {
			return
		}
		<-time.After(5 * time.Millisecond)
	}
	t.Fatalf("fewer than %d %s connections waited on a lock within %v", waiters, application, within)
}

// Settings are bff's connection settings for one login on this database, verified against the test authority.
func (d *Database) Settings(login string) db.Config {
	port, _ := strconv.Atoi(d.settings[PortVariable])
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(d.settings[CAVariable]))
	return db.Config{
		Host: d.settings[HostVariable], Port: port, Name: d.Name, User: login,
		Password: d.settings[LoginsVariable], TLSMode: db.VerifyFull, CA: roots, PoolMax: 2,
	}
}

// Environ is the bff environment for a binary under test: the database settings of every login on this database.
func (d *Database) Environ() []string {
	port, _ := strconv.Atoi(d.settings[PortVariable])
	return environOf(d.settings[HostVariable], port, d.Name, d.settings[CAVariable], d.settings[LoginsVariable])
}

// ClosedPort is a loopback port with nothing listening.
func ClosedPort(t testing.TB) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(port)
	return number
}

// environOf writes the settings of all three logins; without a CA it is the plaintext test mode.
func environOf(host string, port int, name, caPEM, password string) []string {
	environ := []string{
		"DB_HOST=" + host, "DB_PORT=" + strconv.Itoa(port), "DB_NAME=" + name,
		"DB_USER=" + RuntimeLogin, "DB_PASSWORD=" + password,
		"MIGRATION_DB_USER=" + MigrateLogin, "MIGRATION_DB_PASSWORD=" + password,
		"LIFECYCLE_DB_USER=" + LifecycleLogin, "LIFECYCLE_DB_PASSWORD=" + password,
	}
	if caPEM == "" {
		return append(environ, "ENVIRONMENT=test", "DB_TLS_MODE=disable")
	}
	return append(environ, "DB_TLS_CA="+caPEM)
}

// Admin connects to this database as the server's administrator, for test setup and inspection only.
func (d *Database) Admin(t testing.TB) *pgx.Conn {
	t.Helper()
	return d.Connect(t, admin, d.settings[AdminVariable])
}

// Connect opens a direct connection as any login, closed when the test ends.
func (d *Database) Connect(t testing.TB, login, password string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := connect(ctx, d.settings, login, password, d.Name)
	if err != nil {
		t.Fatalf("connect %s: %v", login, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// LoginsPassword is the three logins' password, for a test that creates a login of its own.
func (d *Database) LoginsPassword() string {
	return d.settings[LoginsVariable]
}

func environment(t testing.TB) map[string]string {
	t.Helper()
	settings := map[string]string{}
	for _, name := range []string{HostVariable, PortVariable, CAVariable, AdminVariable, LoginsVariable} {
		value, ok := os.LookupEnv(name)
		if !ok {
			t.Fatalf("%s is not set: run database tests through `go run ./test/withdb -- go test ...` (make int)", name)
		}
		settings[name] = value
	}
	return settings
}

func connect(ctx context.Context, settings map[string]string, login, password, database string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("sslmode=disable")
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(settings[PortVariable], 10, 16)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(settings[CAVariable])) {
		return nil, errors.New("no test authority")
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database = settings[HostVariable], uint16(port), login, password, database
	cfg.TLSConfig = &tls.Config{RootCAs: roots, ServerName: settings[HostVariable], MinVersion: tls.VersionTLS12}
	cfg.Fallbacks = nil
	return pgx.ConnectConfig(ctx, cfg)
}

// Authority is a test certificate authority made for one run; nothing depends on a date.
type Authority struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	pem  string
}

// NewAuthority creates an authority valid for ten years.
func NewAuthority() (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "bff test authority"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Authority{key: key, cert: cert, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}, nil
}

// PEM is the authority's certificate.
func (a *Authority) PEM() string {
	return a.pem
}

// ServerCertificate issues the servers' certificate for the addresses a client dials (127.0.0.1, ::1,
// host.docker.internal, and the names the test services have on their own network) and deliberately not localhost, so
// dialling localhost is a wrong-name case.
func (a *Authority) ServerCertificate() (certPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "bff test database"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		DNSNames:    []string{"host.docker.internal", "postgres", "idp.test", "app.test", "other.test"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), nil
}

// Open opens a pool of kind with settings and closes it when the test ends.
func Open(t testing.TB, settings db.Config, kind db.Kind) *db.Pool {
	t.Helper()
	pool, err := db.Open(settings, kind)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// DevEnviron is the database settings of the run's template database, for `make dev`: the runner set the server's
// variables in this process's environment.
func DevEnviron() []string {
	port, _ := strconv.Atoi(os.Getenv(PortVariable))
	return environOf(os.Getenv(HostVariable), port, template, os.Getenv(CAVariable), os.Getenv(LoginsVariable))
}
