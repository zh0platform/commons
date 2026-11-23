// Package testdb gives tests their own PostgreSQL database on the server a withdb runner started: a clone of a
// template that holds what a product's infrastructure provisions (its logins and its schema). A product describes
// itself once, in a Product, which both its runner and its tests use.
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
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Product describes one product's test stack. Project, Variables, Logins, Template, Schema, SchemaOwner and Root are
// required (Check); a zero optional field turns its feature off.
type Product struct {
	Project   string   // start of every Compose project and run-folder name, such as "shop-test-"
	Variables string   // start of every variable the runner sets but the Keycloak and Compose names, such as "SHOP_TEST_"
	Logins    []string // the server's logins, all with the run's login password
	// Statements run as the administrator after the logins exist: an attribute by ALTER ROLE, a role without login, a
	// grant. They never carry the password.
	Statements  []string
	Template    string                      // the template database every test database is cloned from
	Schema      string                      // the schema created in the template
	SchemaOwner string                      // the login that owns Schema; one of Logins
	Names       []string                    // DNS names on the server certificate besides the module's own
	Migrate     func(testing.TB, *Database) // the product's migration step for one test database
	Refused     func(name string) bool      // inherited variables no process the runner starts may get
	Root        string                      // absolute folder the product's own Compose files resolve from
	Realms      string                      // absolute folder of the Keycloak realm files; empty: no Keycloak
	Files       []string                    // the product's own Compose files, after the module's, relative to Root
	Quiet       bool                        // a failed run prints no service logs
}

var (
	projectForm  = regexp.MustCompile(`^[a-z][a-z0-9-]*-$`)
	variableForm = regexp.MustCompile(`^[A-Z][A-Z0-9_]*_$`)
	sqlName      = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

// Check reports the first field of the description that a stack cannot be built from, by its name.
func (p Product) Check() error {
	switch {
	case !projectForm.MatchString(p.Project):
		return fmt.Errorf("field Project %q: a lowercase letter, then lowercase letters, digits and '-', ending in '-'", p.Project)
	case !variableForm.MatchString(p.Variables):
		return fmt.Errorf("field Variables %q: upper-case letters, digits and '_', ending in '_'", p.Variables)
	case len(p.Logins) == 0:
		return errors.New("field Logins: at least one login")
	case !slices.Contains(p.Logins, p.SchemaOwner):
		return fmt.Errorf("field SchemaOwner %q: one of Logins", p.SchemaOwner)
	case slices.Contains(p.Names, "localhost"):
		return errors.New(`field Names: "localhost" stays off the certificate, so dialling it is the wrong-name case`)
	case !filepath.IsAbs(p.Root):
		return fmt.Errorf("field Root %q: an absolute folder", p.Root)
	}
	fields := []struct{ field, name string }{{"Template", p.Template}, {"Schema", p.Schema}}
	for _, login := range p.Logins {
		fields = append(fields, struct{ field, name string }{"Logins", login})
	}
	for _, f := range fields {
		if !sqlName.MatchString(f.name) || f.name == admin {
			return fmt.Errorf("field %s %q: a lowercase SQL name other than %s", f.field, f.name, admin)
		}
	}
	return nil
}

// PlaintextPrefix starts the names of the databases on which the server accepts the product's logins without TLS
// (the lost-commit tests' relay).
const PlaintextPrefix = "lostcommit_"

// Administrator is the server's superuser, the image's own.
const Administrator = "postgres"

const (
	admin = Administrator
	// cloneLock serializes CREATE DATABASE … TEMPLATE, which fails while another copy of the template runs.
	cloneLock = 7_150_001
)

// Server is where the run's PostgreSQL listens and its two passwords: the administrator's and the logins'.
type Server struct {
	Host, Port, CA, AdminPassword, LoginPassword string
}

// The suffixes of the variables that hand a Server to the command, after the product's Variables.
var serverSuffixes = []string{"DB_HOST", "DB_PORT", "DB_CA", "DB_ADMIN_PASSWORD", "DB_LOGIN_PASSWORD"}

func (s Server) values() []string {
	return []string{s.Host, s.Port, s.CA, s.AdminPassword, s.LoginPassword}
}

// ServerVariables are the names under which a runner hands the Server to its command.
func (p Product) ServerVariables() []string {
	names := make([]string, len(serverSuffixes))
	for i, suffix := range serverSuffixes {
		names[i] = p.Variables + suffix
	}
	return names
}

// ServerEnviron is the Server as NAME=value entries for the command.
func (p Product) ServerEnviron(s Server) []string {
	values := s.values()
	environ := make([]string, len(values))
	for i, name := range p.ServerVariables() {
		environ[i] = name + "=" + values[i]
	}
	return environ
}

// server reads the Server the runner handed to this process; the error names the first missing variable.
func (p Product) server() (Server, error) {
	values := make([]string, len(serverSuffixes))
	for i, name := range p.ServerVariables() {
		value, ok := os.LookupEnv(name)
		if !ok {
			return Server{}, fmt.Errorf("%s is not set: run this through the product's withdb runner", name)
		}
		values[i] = value
	}
	return Server{Host: values[0], Port: values[1], CA: values[2], AdminPassword: values[3], LoginPassword: values[4]}, nil
}

// Provision creates the product's logins, then its statements, then the template with its schema, as the product's
// infrastructure does. It runs with every kind of statement logging off, so no statement that carries the login
// password, failed or not, reaches the server's log, which a failed run prints.
func Provision(ctx context.Context, p Product, s Server) error {
	conn, err := connect(ctx, s, admin, s.AdminPassword, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	// Every setting that can put a statement's text in the log is off for this connection, whatever flags a product
	// gives the server; the transaction sample rate is decided when a transaction starts, so it is set before BEGIN.
	statements := []string{
		"SET log_statement = 'none'",
		"SET log_min_duration_statement = -1",
		"SET log_min_duration_sample = -1",
		"SET log_transaction_sample_rate = 0",
		"SET log_min_error_statement = panic",
		"BEGIN",
	}
	for _, login := range p.Logins {
		statements = append(statements, "CREATE ROLE "+login+" LOGIN PASSWORD '"+s.LoginPassword+"'")
	}
	statements = append(statements, "COMMIT")
	statements = append(statements, p.Statements...)
	statements = append(statements, "CREATE DATABASE "+p.Template+" TEMPLATE template0")
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return err
		}
	}
	tmpl, err := connect(ctx, s, admin, s.AdminPassword, p.Template)
	if err != nil {
		return err
	}
	defer func() { _ = tmpl.Close(ctx) }()
	_, err = tmpl.Exec(ctx, "CREATE SCHEMA "+p.Schema+" AUTHORIZATION "+p.SchemaOwner)
	return err
}

// Database is one test's own database.
type Database struct {
	Name    string
	product Product
	server  Server
}

// New clones the template into a database only this test uses, closed like infrastructure's (CONNECT for the
// product's logins only), and drops it when the test ends. A missing server is a setup failure, never a skip.
func New(t testing.TB, p Product) *Database {
	t.Helper()
	return newNamed(t, p, "test_")
}

// NewMigrated is New with the product's migration step applied.
func NewMigrated(t testing.TB, p Product) *Database {
	t.Helper()
	database := New(t, p)
	database.Migrate(t)
	return database
}

// Migrate applies the product's migration step to this database.
func (d *Database) Migrate(t testing.TB) {
	t.Helper()
	if d.product.Migrate == nil {
		t.Fatal("the product has no migration step (Product.Migrate)")
	}
	d.product.Migrate(t, d)
}

// NewPlaintext is New for a database the server also accepts without TLS (the lost-commit tests' relay).
func NewPlaintext(t testing.TB, p Product) *Database {
	t.Helper()
	return newNamed(t, p, PlaintextPrefix)
}

// TemplateDatabase is the run's template, for a command that is not a test (a development server, a seeder). It is
// not dropped.
func TemplateDatabase(p Product) (*Database, error) {
	s, err := p.server()
	if err != nil {
		return nil, err
	}
	return &Database{Name: p.Template, product: p, server: s}, nil
}

func newNamed(t testing.TB, p Product, prefix string) *Database {
	t.Helper()
	s, err := p.server()
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	d := &Database{Name: prefix + hex.EncodeToString(raw), product: p, server: s}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := connect(ctx, s, admin, s.AdminPassword, "postgres")
	if err != nil {
		t.Fatalf("test database server: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	t.Cleanup(func() { d.drop(t) })
	statements := []string{
		"SELECT pg_advisory_lock(" + strconv.Itoa(cloneLock) + ")",
		"CREATE DATABASE " + d.Name + " TEMPLATE " + p.Template,
		"SELECT pg_advisory_unlock(" + strconv.Itoa(cloneLock) + ")",
		"REVOKE ALL ON DATABASE " + d.Name + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + d.Name + " TO " + strings.Join(p.Logins, ", "),
	}
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("create test database: %v", err)
		}
	}
	return d
}

func (d *Database) drop(t testing.TB) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := connect(ctx, d.server, admin, d.server.AdminPassword, "postgres")
	if err != nil {
		t.Errorf("drop %s: %v", d.Name, err)
		return
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+d.Name+" WITH (FORCE)"); err != nil {
		t.Errorf("drop %s: %v", d.Name, err)
	}
}

// RestoreLikeInfra dumps this database and restores it the way infrastructure's restore does: pg_dump in custom
// format, a target created closed from template0 (CONNECT for the given logins only), pg_restore in one transaction
// with owners and grants. Both programs run inside the server's container. The copy is dropped when the test ends.
func (d *Database) RestoreLikeInfra(t testing.TB, logins ...string) *Database {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := connect(ctx, d.server, admin, d.server.AdminPassword, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	target := &Database{Name: "restored_" + d.Name, product: d.product, server: d.server}
	dump := "/tmp/" + d.Name + ".dump"
	t.Cleanup(func() { target.drop(t) })
	statements := []string{
		"COPY (SELECT 1) TO PROGRAM 'pg_dump --format=custom --file=" + dump + " --dbname=" + d.Name + "'",
		"CREATE DATABASE " + target.Name + " OWNER " + admin + " TEMPLATE template0 ALLOW_CONNECTIONS false",
		"REVOKE ALL ON DATABASE " + target.Name + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + target.Name + " TO " + strings.Join(logins, ", "),
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

// Settings are the plain connection settings of one login on a database; an empty CA means plaintext.
type Settings struct {
	Host     string
	Port     int
	Database string
	Login    string
	Password string
	CA       string // the authority's certificate, PEM
}

// Settings connect a login to this database with TLS, verified against the run's authority.
func (d *Database) Settings(login string) Settings {
	port, _ := strconv.Atoi(d.server.Port)
	return Settings{
		Host: d.server.Host, Port: port, Database: d.Name, Login: login,
		Password: d.server.LoginPassword, CA: d.server.CA,
	}
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

// Admin connects to this database as the server's administrator, for test setup and inspection only.
func (d *Database) Admin(t testing.TB) *pgx.Conn {
	t.Helper()
	return d.Connect(t, admin, d.server.AdminPassword)
}

// Connect opens a direct connection as any login, closed when the test ends.
func (d *Database) Connect(t testing.TB, login, password string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := connect(ctx, d.server, login, password, d.Name)
	if err != nil {
		t.Fatalf("connect %s: %v", login, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// LoginsPassword is the logins' password, for a test that creates a login of its own.
func (d *Database) LoginsPassword() string {
	return d.server.LoginPassword
}

func connect(ctx context.Context, s Server, login, password, database string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("sslmode=disable")
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(s.Port, 10, 16)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(s.CA)) {
		return nil, errors.New("no test authority")
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database = s.Host, uint16(port), login, password, database
	cfg.TLSConfig = &tls.Config{RootCAs: roots, ServerName: s.Host, MinVersion: tls.VersionTLS12}
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
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test authority"},
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

// ServerCertificate issues a server certificate for the loopback addresses (127.0.0.1, ::1), host.docker.internal
// (where containers reach this machine) and the given names, and deliberately not localhost, so dialling localhost is
// a wrong-name case.
func (a *Authority) ServerCertificate(names ...string) (certPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "test server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		DNSNames:    append([]string{"host.docker.internal"}, names...),
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
