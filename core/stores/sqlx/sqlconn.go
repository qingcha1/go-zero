package sqlx

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"github.com/zeromicro/go-zero/core/breaker"
	"github.com/zeromicro/go-zero/core/logx"
)

// spanName is used to identify the span name for the SQL execution.
const spanName = "sql"

// ErrNotFound is an alias of sql.ErrNoRows
var (
	ErrNotFound     = sql.ErrNoRows
	DefaultMatchSql = "SELECT"
)

type (
	// Session stands for raw connections or transaction sessions
	Session interface {
		Exec(query string, args ...interface{}) (sql.Result, error)
		ExecCtx(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
		Prepare(query string) (StmtSession, error)
		PrepareCtx(ctx context.Context, query string) (StmtSession, error)
		QueryRow(v interface{}, query string, args ...interface{}) error
		QueryRowCtx(ctx context.Context, v interface{}, query string, args ...interface{}) error
		QueryRowPartial(v interface{}, query string, args ...interface{}) error
		QueryRowPartialCtx(ctx context.Context, v interface{}, query string, args ...interface{}) error
		QueryRows(v interface{}, query string, args ...interface{}) error
		QueryRowsCtx(ctx context.Context, v interface{}, query string, args ...interface{}) error
		QueryRowsPartial(v interface{}, query string, args ...interface{}) error
		QueryRowsPartialCtx(ctx context.Context, v interface{}, query string, args ...interface{}) error
	}

	// SqlConn only stands for raw connections, so Transact method can be called.
	SqlConn interface {
		Session
		// RawDB is for other ORM to operate with, use it with caution.
		// Notice: don't close it.
		RawDB() (*sql.DB, error)
		Transact(fn func(Session) error) error
		TransactCtx(ctx context.Context, fn func(context.Context, Session) error) error
	}

	// SqlOption defines the method to customize a sql connection.
	SqlOption func(*commonSqlConn)

	// StmtSession interface represents a session that can be used to execute statements.
	StmtSession interface {
		Close() error
		Exec(args ...interface{}) (sql.Result, error)
		ExecCtx(ctx context.Context, args ...interface{}) (sql.Result, error)
		QueryRow(v interface{}, args ...interface{}) error
		QueryRowCtx(ctx context.Context, v interface{}, args ...interface{}) error
		QueryRowPartial(v interface{}, args ...interface{}) error
		QueryRowPartialCtx(ctx context.Context, v interface{}, args ...interface{}) error
		QueryRows(v interface{}, args ...interface{}) error
		QueryRowsCtx(ctx context.Context, v interface{}, args ...interface{}) error
		QueryRowsPartial(v interface{}, args ...interface{}) error
		QueryRowsPartialCtx(ctx context.Context, v interface{}, args ...interface{}) error
	}

	// thread-safe
	// Because CORBA doesn't support PREPARE, so we need to combine the
	// query arguments into one string and do underlying query without arguments
	commonSqlConn struct {
		connProv    connProvider
		onError     func(string, error)
		datasource  map[string]string // 在集群模式启用情况下，指定 `master` 和 `slave` 数据库连接信息
		cluster     bool              // 在集群模式启用情况下，是否启用读写分离
		clusterMode bool              // 标识当前是单表模式还是集群模式
		beginTx     beginnable
		brk         breaker.Breaker
		accept      func(error) bool
	}

	connProvider func(ds string) (*sql.DB, error)

	sessionConn interface {
		Exec(query string, args ...interface{}) (sql.Result, error)
		ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
		Query(query string, args ...interface{}) (*sql.Rows, error)
		QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	}

	statement struct {
		query string
		stmt  *sql.Stmt
	}

	stmtConn interface {
		Exec(args ...interface{}) (sql.Result, error)
		ExecContext(ctx context.Context, args ...interface{}) (sql.Result, error)
		Query(args ...interface{}) (*sql.Rows, error)
		QueryContext(ctx context.Context, args ...interface{}) (*sql.Rows, error)
	}
)

// NewSqlConn returns a SqlConn with given driver name and datasource.
func NewSqlConn(driverName string, datasource string, opts ...SqlOption) SqlConn {
	conn := &commonSqlConn{
		connProv: func(ds string) (*sql.DB, error) {
			return getSqlConn(driverName, datasource)
		},
		onError: func(ds string, err error) {
			logInstanceError(datasource, err)
		},
		datasource:  map[string]string{},
		cluster:     false,
		clusterMode: false,
		beginTx:     begin,
		brk:         breaker.NewBreaker(),
	}
	for _, opt := range opts {
		opt(conn)
	}

	return conn
}

// NewSqlConnCluster returns a SqlConn with given driver name and datasource.
func NewSqlConnCluster(driverName string, datasource map[string]string, cluster bool, opts ...SqlOption) SqlConn {
	conn := &commonSqlConn{
		connProv: func(ds string) (*sql.DB, error) {
			return getSqlConn(driverName, ds)
		},
		onError: func(ds string, err error) {
			logInstanceError(ds, err)
		},
		datasource:  datasource,
		cluster:     cluster,
		clusterMode: true,
		beginTx:     begin,
		brk:         breaker.NewBreaker(),
	}
	for _, opt := range opts {
		opt(conn)
	}

	return conn
}

// TODO not used.
// NewSqlConnFromDB returns a SqlConn with the given sql.DB.
// Use it with caution, it's provided for other ORM to interact with.
func NewSqlConnFromDB(db *sql.DB, opts ...SqlOption) SqlConn {
	conn := &commonSqlConn{
		connProv: func(ds string) (*sql.DB, error) {
			return db, nil
		},
		onError: func(ds string, err error) {
			logx.Errorf("Error on getting sql instance: dataSource %v error %v", ds, err)
		},
		beginTx: begin,
		brk:     breaker.NewBreaker(),
	}
	for _, opt := range opts {
		opt(conn)
	}

	return conn
}

func (db *commonSqlConn) DataSourceResp(query string) (string, error) {
	if db.clusterMode {
		if db.cluster {
			reg := regexp.MustCompile(`(?U)^.* `).FindAllString(strings.TrimSpace(query), -1)
			if strings.ToUpper(strings.TrimSpace(reg[0])) == DefaultMatchSql {
				if _, ok := db.datasource["slave"]; ok {
					return db.datasource["slave"], nil
				} else {
					return "", errors.New("dataSource config slave error")
				}
			}
		}

		if _, ok := db.datasource["master"]; ok { //only use master
			return db.datasource["master"], nil
		} else {
			return "", errors.New("dataSource config master error")
		}
	} else {
		return "", nil
	}
}

func (db *commonSqlConn) Exec(q string, args ...interface{}) (result sql.Result, err error) {
	return db.ExecCtx(context.Background(), q, args...)
}

func (db *commonSqlConn) ExecCtx(ctx context.Context, q string, args ...interface{}) (
	result sql.Result, err error) {
	ctx, span := startSpan(ctx, "Exec")
	defer func() {
		endSpan(span, err)
	}()

	err = db.brk.DoWithAcceptable(func() error {
		datasource, err := db.DataSourceResp(q)
		logx.Infof("exec DataSourceResp data %v,%v,%v,%v", q, db.cluster, db.datasource, datasource)
		if err != nil {
			logInstanceError(datasource, err)
			return err
		}

		var conn *sql.DB
		conn, err = db.connProv(datasource)
		if err != nil {
			db.onError(datasource, err)
			return err
		}

		result, err = exec(ctx, conn, q, args...)
		return err
	}, db.acceptable)

	return
}

func (db *commonSqlConn) Prepare(query string) (stmt StmtSession, err error) {
	return db.PrepareCtx(context.Background(), query)
}

func (db *commonSqlConn) PrepareCtx(ctx context.Context, query string) (stmt StmtSession, err error) {
	ctx, span := startSpan(ctx, "Prepare")
	defer func() {
		endSpan(span, err)
	}()

	err = db.brk.DoWithAcceptable(func() error {
		datasource, err := db.DataSourceResp(query)
		logx.Infof("exec DataSourceResp data %v,%v,%v,%v", query, db.cluster, db.datasource, datasource)
		if err != nil {
			logInstanceError(datasource, err)
			return err
		}

		var conn *sql.DB
		conn, err = db.connProv(datasource)
		if err != nil {
			db.onError(datasource, err)
			return err
		}

		st, err := conn.PrepareContext(ctx, query)
		if err != nil {
			return err
		}

		stmt = statement{
			query: query,
			stmt:  st,
		}
		return nil
	}, db.acceptable)

	return
}

func (db *commonSqlConn) QueryRow(v interface{}, q string, args ...interface{}) error {
	return db.QueryRowCtx(context.Background(), v, q, args...)
}

func (db *commonSqlConn) QueryRowCtx(ctx context.Context, v interface{}, q string,
	args ...interface{}) (err error) {
	ctx, span := startSpan(ctx, "QueryRow")
	defer func() {
		endSpan(span, err)
	}()

	return db.queryRows(ctx, func(rows *sql.Rows) error {
		return unmarshalRow(v, rows, true)
	}, q, args...)
}

func (db *commonSqlConn) QueryRowPartial(v interface{}, q string, args ...interface{}) error {
	return db.QueryRowPartialCtx(context.Background(), v, q, args...)
}

func (db *commonSqlConn) QueryRowPartialCtx(ctx context.Context, v interface{},
	q string, args ...interface{}) (err error) {
	ctx, span := startSpan(ctx, "QueryRowPartial")
	defer func() {
		endSpan(span, err)
	}()

	return db.queryRows(ctx, func(rows *sql.Rows) error {
		return unmarshalRow(v, rows, false)
	}, q, args...)
}

func (db *commonSqlConn) QueryRows(v interface{}, q string, args ...interface{}) error {
	return db.QueryRowsCtx(context.Background(), v, q, args...)
}

func (db *commonSqlConn) QueryRowsCtx(ctx context.Context, v interface{}, q string,
	args ...interface{}) (err error) {
	ctx, span := startSpan(ctx, "QueryRows")
	defer func() {
		endSpan(span, err)
	}()

	return db.queryRows(ctx, func(rows *sql.Rows) error {
		return unmarshalRows(v, rows, true)
	}, q, args...)
}

func (db *commonSqlConn) QueryRowsPartial(v interface{}, q string, args ...interface{}) error {
	return db.QueryRowsPartialCtx(context.Background(), v, q, args...)
}

func (db *commonSqlConn) QueryRowsPartialCtx(ctx context.Context, v interface{},
	q string, args ...interface{}) (err error) {
	ctx, span := startSpan(ctx, "QueryRowsPartial")
	defer func() {
		endSpan(span, err)
	}()

	return db.queryRows(ctx, func(rows *sql.Rows) error {
		return unmarshalRows(v, rows, false)
	}, q, args...)
}

func (db *commonSqlConn) RawDB() (*sql.DB, error) {
	// TODO not used.
	return db.connProv("")
}

func (db *commonSqlConn) Transact(fn func(Session) error) error {
	return db.TransactCtx(context.Background(), func(_ context.Context, session Session) error {
		return fn(session)
	})
}

func (db *commonSqlConn) TransactCtx(ctx context.Context, fn func(context.Context, Session) error) (err error) {
	ctx, span := startSpan(ctx, "Transact")
	defer func() {
		endSpan(span, err)
	}()

	return db.brk.DoWithAcceptable(func() error {
		return transact(ctx, db, db.beginTx, fn)
	}, db.acceptable)
}

func (db *commonSqlConn) acceptable(err error) bool {
	ok := err == nil || err == sql.ErrNoRows || err == sql.ErrTxDone || err == context.Canceled
	if db.accept == nil {
		return ok
	}

	return ok || db.accept(err)
}

func (db *commonSqlConn) queryRows(ctx context.Context, scanner func(*sql.Rows) error,
	q string, args ...interface{}) (err error) {
	var qerr error
	return db.brk.DoWithAcceptable(func() error {
		datasource, err := db.DataSourceResp(q)
		logx.Infof("exec DataSourceResp data %v,%v,%v,%v", q, db.cluster, db.datasource, datasource)
		if err != nil {
			logInstanceError(datasource, err)
			return err
		}

		conn, err := db.connProv(datasource)
		if err != nil {
			db.onError(datasource, err)
			return err
		}

		return query(ctx, conn, func(rows *sql.Rows) error {
			qerr = scanner(rows)
			return qerr
		}, q, args...)
	}, func(err error) bool {
		return qerr == err || db.acceptable(err)
	})
}

func (s statement) Close() error {
	return s.stmt.Close()
}

func (s statement) Exec(args ...interface{}) (sql.Result, error) {
	return s.ExecCtx(context.Background(), args...)
}

func (s statement) ExecCtx(ctx context.Context, args ...interface{}) (result sql.Result, err error) {
	ctx, span := startSpan(ctx, "Exec")
	defer func() {
		endSpan(span, err)
	}()

	return execStmt(ctx, s.stmt, s.query, args...)
}

func (s statement) QueryRow(v interface{}, args ...interface{}) error {
	return s.QueryRowCtx(context.Background(), v, args...)
}

func (s statement) QueryRowCtx(ctx context.Context, v interface{}, args ...interface{}) (err error) {
	ctx, span := startSpan(ctx, "QueryRow")
	defer func() {
		endSpan(span, err)
	}()

	return queryStmt(ctx, s.stmt, func(rows *sql.Rows) error {
		return unmarshalRow(v, rows, true)
	}, s.query, args...)
}

func (s statement) QueryRowPartial(v interface{}, args ...interface{}) error {
	return s.QueryRowPartialCtx(context.Background(), v, args...)
}

func (s statement) QueryRowPartialCtx(ctx context.Context, v interface{}, args ...interface{}) (err error) {
	ctx, span := startSpan(ctx, "QueryRowPartial")
	defer func() {
		endSpan(span, err)
	}()

	return queryStmt(ctx, s.stmt, func(rows *sql.Rows) error {
		return unmarshalRow(v, rows, false)
	}, s.query, args...)
}

func (s statement) QueryRows(v interface{}, args ...interface{}) error {
	return s.QueryRowsCtx(context.Background(), v, args...)
}

func (s statement) QueryRowsCtx(ctx context.Context, v interface{}, args ...interface{}) (err error) {
	ctx, span := startSpan(ctx, "QueryRows")
	defer func() {
		endSpan(span, err)
	}()

	return queryStmt(ctx, s.stmt, func(rows *sql.Rows) error {
		return unmarshalRows(v, rows, true)
	}, s.query, args...)
}

func (s statement) QueryRowsPartial(v interface{}, args ...interface{}) error {
	return s.QueryRowsPartialCtx(context.Background(), v, args...)
}

func (s statement) QueryRowsPartialCtx(ctx context.Context, v interface{}, args ...interface{}) (err error) {
	ctx, span := startSpan(ctx, "QueryRowsPartial")
	defer func() {
		endSpan(span, err)
	}()

	return queryStmt(ctx, s.stmt, func(rows *sql.Rows) error {
		return unmarshalRows(v, rows, false)
	}, s.query, args...)
}
