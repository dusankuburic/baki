package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeSQL is a minimal database/sql driver for unit tests that must see WHICH
// statements a storage method sends, and on WHICH pooled connection, without
// a Postgres server (none is available to these tests). It answers through a
// scripted respond func and records every call. It parses nothing: tests
// assert on the SQL text the backend produced.
type fakeSQL struct {
	mu      sync.Mutex
	calls   []fakeCall
	conns   int
	respond func(query string, args []driver.NamedValue) fakeResult
}

// fakeCall is one statement as the driver received it.
type fakeCall struct {
	conn  int // 1-based id of the pooled connection that ran it
	inTx  bool
	query string
	args  []driver.NamedValue
}

// fakeResult is a scripted answer: a row set for queries, or an error.
type fakeResult struct {
	cols []string
	rows [][]driver.Value
	err  error
}

// newFakeBackend returns a backend over a fake pool capped at maxOpen
// connections. respond may be nil (every query returns no rows).
func newFakeBackend(t *testing.T, maxOpen int, respond func(query string, args []driver.NamedValue) fakeResult) (*PostgresStorageBackend, *fakeSQL) {
	t.Helper()
	f := &fakeSQL{respond: respond}
	db := sql.OpenDB(fakeConnector{f: f})
	db.SetMaxOpenConns(maxOpen)
	t.Cleanup(func() { _ = db.Close() })
	return &PostgresStorageBackend{db: db}, f
}

func (f *fakeSQL) answer(c *fakeConn, query string, args []driver.NamedValue) fakeResult {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{conn: c.id, inTx: c.inTx, query: query, args: args})
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		return fakeResult{}
	}
	return respond(query, args)
}

// Calls returns a snapshot of the recorded statements (BEGIN/COMMIT/ROLLBACK
// included).
func (f *fakeSQL) Calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

// Queries returns the recorded statements other than transaction control.
func (f *fakeSQL) Queries() []fakeCall {
	var out []fakeCall
	for _, c := range f.Calls() {
		switch c.query {
		case "BEGIN", "COMMIT", "ROLLBACK":
			continue
		}
		out = append(out, c)
	}
	return out
}

type fakeConnector struct{ f *fakeSQL }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	c.f.mu.Lock()
	c.f.conns++
	id := c.f.conns
	c.f.mu.Unlock()
	return &fakeConn{f: c.f, id: id}, nil
}

func (c fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fakesql: open through the connector")
}

type fakeConn struct {
	f    *fakeSQL
	id   int
	inTx bool
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fakesql: prepared statements are not supported")
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *fakeConn) BeginTx(_ context.Context, _ driver.TxOptions) (driver.Tx, error) {
	c.f.answer(c, "BEGIN", nil)
	c.inTx = true
	return &fakeTx{c: c}, nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	r := c.f.answer(c, query, args)
	if r.err != nil {
		return nil, r.err
	}
	return &fakeRows{cols: r.cols, rows: r.rows}, nil
}

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	r := c.f.answer(c, query, args)
	if r.err != nil {
		return nil, r.err
	}
	return driver.RowsAffected(1), nil
}

// CheckNamedValue accepts every argument as is ([]string included, which pgx
// encodes as a Postgres array and the default converter would reject).
func (c *fakeConn) CheckNamedValue(*driver.NamedValue) error { return nil }

type fakeTx struct{ c *fakeConn }

func (t *fakeTx) Commit() error {
	t.c.inTx = false
	t.c.f.answer(t.c, "COMMIT", nil)
	return nil
}

func (t *fakeTx) Rollback() error {
	t.c.inTx = false
	t.c.f.answer(t.c, "ROLLBACK", nil)
	return nil
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

// selectList returns the comma-separated items between the first SELECT and
// the following FROM, trimmed — enough to inspect which columns a single-table
// read names.
func selectList(query string) []string {
	upper := strings.ToUpper(query)
	start := strings.Index(upper, "SELECT")
	end := strings.Index(upper, " FROM ")
	if start < 0 || end < start {
		return nil
	}
	var out []string
	for _, item := range strings.Split(query[start+len("SELECT"):end], ",") {
		out = append(out, strings.TrimSpace(item))
	}
	return out
}

// oneLine collapses a query's whitespace so assertions are not tied to the
// source's indentation.
func oneLine(query string) string { return strings.Join(strings.Fields(query), " ") }
