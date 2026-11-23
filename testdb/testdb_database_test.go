//go:build database

package testdb_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"git.kloudbox.de/commons/commons/internal/selftest"
	"git.kloudbox.de/commons/commons/testdb"
)

func product(t *testing.T) testdb.Product {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return selftest.Product(root)
}

// The second user's provisioning, as its infrastructure does it: its four logins, the one that bypasses row
// security, the role without login granted with SET and without INHERIT, its schema owned by its migration login, and
// test databases closed to everyone but its logins.
func TestProvisioningMatchesTheProductsDescription(t *testing.T) {
	database := testdb.NewMigrated(t, product(t))
	admin := database.Admin(t)
	ctx := context.Background()
	var bypass, schedulerLogin, inherit, set bool
	var owner string
	if err := admin.QueryRow(ctx, `SELECT
		(SELECT rolbypassrls FROM pg_roles WHERE rolname = $1),
		(SELECT rolcanlogin FROM pg_roles WHERE rolname = $2),
		m.inherit_option, m.set_option,
		(SELECT nspowner::regrole::text FROM pg_namespace WHERE nspname = 'agent')
		FROM pg_auth_members m WHERE m.roleid = $2::regrole AND m.member = $3::regrole`,
		selftest.RecoveryLogin, selftest.SchedulerRole, selftest.MigrateLogin).
		Scan(&bypass, &schedulerLogin, &inherit, &set, &owner); err != nil {
		t.Fatal(err)
	}
	if !bypass || schedulerLogin || inherit || !set || owner != selftest.MigrateLogin {
		t.Errorf("roles: recovery bypasses row security %v, scheduler logs in %v, its grant inherits %v and sets %v, schema owner %s; want true, false, false, true, %s",
			bypass, schedulerLogin, inherit, set, owner, selftest.MigrateLogin)
	}
	var migrated, publicConnects bool
	if err := admin.QueryRow(ctx, `SELECT to_regclass('agent.marker') IS NOT NULL,
		has_database_privilege('public', current_database(), 'CONNECT')`).Scan(&migrated, &publicConnects); err != nil {
		t.Fatal(err)
	}
	if !migrated || publicConnects {
		t.Errorf("migrated %v, CONNECT for PUBLIC %v; want true, false", migrated, publicConnects)
	}
	conn := database.Connect(t, selftest.RuntimeLogin, database.LoginsPassword())
	if err := conn.Ping(ctx); err != nil {
		t.Errorf("the runtime login cannot connect: %v", err)
	}
}

// The restored copy holds the data and lets only the given logins in.
func TestRestoreLikeInfraClosesTheCopyToTheGivenLogins(t *testing.T) {
	restored := testdb.NewMigrated(t, product(t)).RestoreLikeInfra(t, selftest.MigrateLogin)
	var table, runtimeConnects bool
	if err := restored.Admin(t).QueryRow(context.Background(), `SELECT to_regclass('agent.marker') IS NOT NULL,
		has_database_privilege($1, current_database(), 'CONNECT')`, selftest.RuntimeLogin).Scan(&table, &runtimeConnects); err != nil {
		t.Fatal(err)
	}
	if !table || runtimeConnects {
		t.Errorf("restored copy: table %v, CONNECT for the runtime login %v; want true, false", table, runtimeConnects)
	}
}

// A test's database and relay end with it: the database is dropped and a connection the relay accepted is closed,
// even one that never sent a byte.
func TestDatabaseAndRelayEndWithTheirTest(t *testing.T) {
	observer := testdb.New(t, product(t)).Admin(t)
	var name string
	var idle net.Conn
	t.Run("owner", func(owner *testing.T) {
		database := testdb.NewPlaintext(owner, product(owner))
		name = database.Name
		idle = dial(t, testdb.NewForwarder(owner, database)) // closed by the outer test, after the owner's cleanup
	})
	assertClosed(t, idle)
	var exists bool
	if err := observer.QueryRow(context.Background(), "SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Errorf("database %s still exists after its test ended", name)
	}
}

// A message declaring a length above the relay's cap closes that connection's two sockets; another connection
// through the same relay goes on.
func TestRelayClosesAConnectionWhoseMessageExceedsTheCap(t *testing.T) {
	database := testdb.NewPlaintext(t, product(t))
	relay := testdb.NewForwarder(t, database)
	oversized := dial(t, relay)
	header := binary.BigEndian.AppendUint32(nil, 1<<20+1)
	if _, err := oversized.Write(header); err != nil {
		t.Fatal(err)
	}
	assertClosed(t, oversized)
	settings := relay.Settings(selftest.RuntimeLogin)
	config, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=disable",
		settings.Host, settings.Port, settings.Database, settings.Login))
	if err != nil {
		t.Fatal(err)
	}
	config.Password = settings.Password
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect through the relay after an oversized message: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := conn.Ping(ctx); err != nil {
		t.Errorf("a query through the relay: %v", err)
	}
}

// Over TCP without TLS the server admits the product's plaintext logins on a plaintext-prefix database and nothing
// else; the login it refuses there still connects with TLS.
func TestPlaintextAdmitsOnlyTheListedLogins(t *testing.T) {
	plaintext := testdb.NewPlaintext(t, product(t))
	ordinary := testdb.New(t, product(t))
	for _, c := range []struct {
		database *testdb.Database
		login    string
		admitted bool
	}{
		{plaintext, selftest.RuntimeLogin, true},
		{plaintext, selftest.RecoveryLogin, false},
		{ordinary, selftest.RuntimeLogin, false},
	} {
		err := connectWithoutTLS(c.database.Settings(c.login))
		var refused *pgconn.PgError
		if c.admitted && err != nil || !c.admitted && !(errors.As(err, &refused) && refused.Code == "28000") {
			t.Errorf("connect %s to %s without TLS = %v, want admitted %v (a refusal is SQLSTATE 28000)", c.login, c.database.Name, err, c.admitted)
		}
	}
	plaintext.Connect(t, selftest.RecoveryLogin, plaintext.LoginsPassword())
}

func connectWithoutTLS(settings testdb.Settings) error {
	config, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=disable",
		settings.Host, settings.Port, settings.Database, settings.Login))
	if err != nil {
		return err
	}
	config.Password = settings.Password
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return err
	}
	return conn.Close(ctx)
}

func dial(t *testing.T, relay *testdb.Forwarder) net.Conn {
	t.Helper()
	settings := relay.Settings(selftest.RuntimeLogin)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port)), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// assertClosed waits until the relay's side of conn is closed.
func assertClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	if timeout, ok := errors.AsType[net.Error](err); err == nil || ok && timeout.Timeout() {
		t.Errorf("read from the relay = %v, want its end (EOF or reset): the relay kept the connection open", err)
	}
}
