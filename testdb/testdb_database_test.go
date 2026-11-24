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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zh0platform/commons/internal/selftest"
	"github.com/zh0platform/commons/testdb"
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

// The restored copy holds the data, except an unlogged table's rows as infrastructure's dump leaves them out, and
// lets only the given logins in.
func TestRestoreLikeInfraClosesTheCopyToTheGivenLogins(t *testing.T) {
	source := testdb.NewMigrated(t, product(t))
	if _, err := source.Admin(t).Exec(context.Background(), `INSERT INTO agent.marker VALUES (1);
		CREATE UNLOGGED TABLE agent.scratch (id int); INSERT INTO agent.scratch VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	restored := source.RestoreLikeInfra(t, selftest.MigrateLogin)
	var markers, scratch int
	var migrateConnects, runtimeConnects bool
	if err := restored.Admin(t).QueryRow(context.Background(), `SELECT (SELECT count(*) FROM agent.marker),
		(SELECT count(*) FROM agent.scratch), has_database_privilege($1, current_database(), 'CONNECT'),
		has_database_privilege($2, current_database(), 'CONNECT')`, selftest.MigrateLogin, selftest.RuntimeLogin).
		Scan(&markers, &scratch, &migrateConnects, &runtimeConnects); err != nil {
		t.Fatal(err)
	}
	if markers != 1 || scratch != 0 || !migrateConnects || runtimeConnects {
		t.Errorf("restored copy: %d marker rows, %d unlogged rows, CONNECT for the given login %v and the runtime login %v; want 1, 0, true, false",
			markers, scratch, migrateConnects, runtimeConnects)
	}
}

// A program on the server is judged by its exit status alone: it gets no rows, so one that never reads its input
// passes and one that fails is still reported. The reason: a row a program does not read breaks the pipe, and a row
// larger than a pipe's buffer does so whatever the timing.
func TestServerProgramsAreJudgedByTheirExitStatus(t *testing.T) {
	conn := testdb.New(t, product(t)).Admin(t)
	ctx := context.Background()
	if err := testdb.RunOnServer(ctx, conn, "test $(wc -c) -eq 0"); err != nil {
		t.Errorf("RunOnServer(a program that fails on any input byte) = %v, want nil", err)
	}
	if err := testdb.RunOnServer(ctx, conn, "false"); err == nil || !strings.Contains(err.Error(), `"false"`) {
		t.Errorf("RunOnServer(false) = %v, want its failure naming the program", err)
	}
	if _, err := conn.Exec(ctx, "COPY (SELECT repeat('x', 1048576)) TO PROGRAM 'true'"); err == nil {
		t.Error("COPY of a 1 MB row to a program that never reads = nil, want a broken pipe")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := plainConnect(relay.Settings(selftest.RuntimeLogin))
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
	conn, err := plainConnect(plaintext.Settings(selftest.RuntimeLogin))
	if err != nil {
		t.Fatalf("connect a listed login to a plaintext database without TLS: %v", err)
	}
	_ = conn.Close(context.Background())
	for _, c := range []struct {
		database *testdb.Database
		login    string
	}{
		{plaintext, selftest.RecoveryLogin},
		{ordinary, selftest.RuntimeLogin},
	} {
		_, err := plainConnect(c.database.Settings(c.login))
		if refused, ok := errors.AsType[*pgconn.PgError](err); !ok || refused.Code != "28000" {
			t.Errorf("connect %s to %s without TLS = %v, want refused (SQLSTATE 28000)", c.login, c.database.Name, err)
		}
	}
	plaintext.Connect(t, selftest.RecoveryLogin, plaintext.LoginsPassword())
}

// An arm that skips one completion lets the first tagged statement through, whichever connection completes it, and
// withholds the second one's answer, on another connection, after its work commits; a completion of another tag
// counts for nothing.
func TestRelayDropsTheAnswerAfterTheSkippedCompletions(t *testing.T) {
	database := testdb.NewPlaintext(t, product(t))
	ctx := context.Background()
	if _, err := database.Admin(t).Exec(ctx, "CREATE TABLE public.relayed (n int); GRANT INSERT ON public.relayed TO "+selftest.RuntimeLogin); err != nil {
		t.Fatal(err)
	}
	relay := testdb.NewForwarder(t, database)
	first, err := plainConnect(relay.Settings(selftest.RuntimeLogin))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close(ctx) }()
	second, err := plainConnect(relay.Settings(selftest.RuntimeLogin))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close(ctx) }()
	relay.DropAnswerAfter("INSERT", 1)
	if _, err := first.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("a completion of another tag: %v, want answered", err)
	}
	if _, err := first.Exec(ctx, "INSERT INTO public.relayed VALUES (1)"); err != nil {
		t.Fatalf("the skipped completion: %v, want answered", err)
	}
	if _, err := second.Exec(ctx, "INSERT INTO public.relayed VALUES (2)"); err == nil {
		t.Fatal("the armed completion was answered, want its answer withheld")
	}
	var rows int
	if err := database.Admin(t).QueryRow(ctx, "SELECT count(*) FROM public.relayed").Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("rows = %d, %v, want both inserts committed", rows, err)
	}
}

// plainConnect connects without TLS, through a relay or straight to a database.
func plainConnect(settings testdb.Settings) (*pgx.Conn, error) {
	config, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=disable",
		settings.Host, settings.Port, settings.Database, settings.Login))
	if err != nil {
		return nil, err
	}
	config.Password = settings.Password
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return pgx.ConnectConfig(ctx, config)
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
