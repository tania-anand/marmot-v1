package db

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/doug-martin/goqu/v9"
	"github.com/fsnotify/fsnotify"
	"github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog/log"
)

var MarmotPrefix = "__marmot__"

type SqliteStreamDB struct {
	*goqu.Database
	rawConnection     *sqlite3.SQLiteConn
	dbPath            string
	watcher           *fsnotify.Watcher
	prefix            string
	publishLock       *sync.Mutex
	watchTablesSchema map[string][]*ColumnInfo
	OnChange          func(event *ChangeLogEvent) error
}

type ColumnInfo struct {
	Name         string `db:"name"`
	Type         string `db:"type"`
	NotNull      bool   `db:"notnull"`
	DefaultValue any    `db:"dflt_value"`
	IsPrimaryKey bool   `db:"pk"`
}

func GetAllDBTables(path string) ([]string, error) {
	connectionStr := fmt.Sprintf("%s?_journal_mode=wal", path)
	conn, rawConn, err := OpenRaw(connectionStr)
	if err != nil {
		return nil, err
	}
	defer rawConn.Close()
	defer conn.Close()

	gSQL := goqu.New("sqlite", conn)

	names := make([]string, 0)
	err = gSQL.Select("name").From("sqlite_schema").Where(
		goqu.C("type").Eq("table"),
		goqu.C("name").NotLike("sqlite_%"),
		goqu.C("name").NotLike(MarmotPrefix+"%"),
	).ScanVals(&names)

	if err != nil {
		return nil, err
	}

	return names, nil
}

func OpenStreamDB(path string, tables []string) (*SqliteStreamDB, error) {
	connectionStr := fmt.Sprintf("%s?_journal_mode=wal", path)
	conn, rawConn, err := OpenRaw(connectionStr)
	if err != nil {
		return nil, err
	}

	conn.SetConnMaxLifetime(0)
	conn.SetConnMaxIdleTime(10 * time.Second)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	err = watcher.Add(path)
	if err != nil {
		return nil, err
	}

	sqliteQu := goqu.Dialect("sqlite3")
	ret := &SqliteStreamDB{
		Database:          sqliteQu.DB(conn),
		rawConnection:     rawConn,
		watcher:           watcher,
		dbPath:            path,
		prefix:            MarmotPrefix,
		publishLock:       &sync.Mutex{},
		watchTablesSchema: map[string][]*ColumnInfo{},
	}

	for _, n := range tables {
		colInfo, err := ret.GetTableInfo(n)
		if err != nil {
			return nil, err
		}

		ret.watchTablesSchema[n] = colInfo
	}

	return ret, nil
}

func OpenRaw(dns string) (*sql.DB, *sqlite3.SQLiteConn, error) {
	var rawConn *sqlite3.SQLiteConn
	d := &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			rawConn = conn
			return conn.RegisterFunc("marmot_version", func() string {
				return "0.1"
			}, true)
		},
	}

	conn := sql.OpenDB(SqliteDriverConnector{driver: d, dns: dns})
	err := conn.Ping()
	if err != nil {
		return nil, nil, err
	}

	return conn, rawConn, nil
}

func (conn *SqliteStreamDB) InstallCDC() error {
	for tableName := range conn.watchTablesSchema {
		err := conn.initTriggers(tableName)
		if err != nil {
			return err
		}
	}

	go conn.watchChanges(conn.dbPath)
	return nil
}

func (conn *SqliteStreamDB) RemoveCDC(tables bool) error {
	log.Info().Msg("Uninstalling all CDC triggers...")
	err := cleanMarmotTriggers(conn.Database, conn.prefix)
	if err != nil {
		return err
	}

	if tables {
		return clearMarmotTables(conn.Database, conn.prefix)
	}

	return nil
}

func (conn *SqliteStreamDB) Execute(query string) error {
	st, err := conn.Prepare(query)
	if err != nil {
		return err
	}

	stmt := &EnhancedStatement{st}
	defer stmt.Finalize()

	if _, err := stmt.Exec(); err != nil {
		return err
	}

	return nil
}

func (conn *SqliteStreamDB) GetTableInfo(table string) ([]*ColumnInfo, error) {
	query := "SELECT name, type, `notnull`, dflt_value, pk FROM pragma_table_info(?)"
	stmt, err := conn.Prepare(query)
	if err != nil {
		return nil, err
	}

	rows, err := stmt.Query(table)
	if err != nil {
		return nil, err
	}

	tableInfo := make([]*ColumnInfo, 0)
	hasPrimaryKey := false
	for rows.Next() {
		if rows.Err() != nil {
			return nil, rows.Err()
		}

		c := ColumnInfo{}
		err = rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.DefaultValue, &c.IsPrimaryKey)
		if err != nil {
			return nil, err
		}

		if c.IsPrimaryKey {
			hasPrimaryKey = true
		}

		tableInfo = append(tableInfo, &c)
	}

	if !hasPrimaryKey {
		tableInfo = append(tableInfo, &ColumnInfo{
			Name:         "rowid",
			IsPrimaryKey: true,
			Type:         "INT",
			NotNull:      true,
			DefaultValue: nil,
		})
	}

	return tableInfo, nil
}

func (conn *SqliteStreamDB) BackupTo(bkFilePath string) error {
	_, src, err := OpenRaw(fmt.Sprintf("%s?_foreign_keys=false&_journal_mode=wal", conn.dbPath))
	if err != nil {
		return err
	}
	defer src.Close()

	_, err = src.Exec("VACUUM main INTO ?;", []driver.Value{bkFilePath})
	if err != nil {
		return err
	}

	if err := src.Close(); err != nil {
		log.Error().Err(err).Msg("Unable to close source DB")
	}

	sqlDB, src, err := OpenRaw(fmt.Sprintf("%s?_foreign_keys=false&_journal_mode=wal", bkFilePath))
	if err != nil {
		return err
	}

	gSQL := goqu.New("sqlite", sqlDB)
	err = cleanMarmotTriggers(gSQL, conn.prefix)
	if err != nil {
		return err
	}

	return nil
}

func (conn *SqliteStreamDB) RestoreFrom(bkFilePath string) error {
	dnsTpl := "%s?_journal_mode=wal&_foreign_keys=false&&_busy_timeout=30000&_txlock=%s"
	dns := fmt.Sprintf(dnsTpl, conn.dbPath, "immediate")
	destDB, dest, err := OpenRaw(dns)
	if err != nil {
		return err
	}
	defer func() {
		_ = dest.Close()
	}()

	dns = fmt.Sprintf(dnsTpl, bkFilePath, "immediate")
	srcDB, src, err := OpenRaw(dns)
	if err != nil {
		return err
	}
	defer func() {
		_ = src.Close()
	}()

	dgSQL := goqu.New("sqlite", destDB)
	sgSQL := goqu.New("sqlite", srcDB)

	// Source locking is required so that any lock related metadata is mirrored in destination
	// Transacting on both src and dest in immediate mode makes sure nobody
	// else is modifying or interacting with DB
	err = sgSQL.WithTx(func(_ *goqu.TxDatabase) error {
		return dgSQL.WithTx(func(_ *goqu.TxDatabase) error {
			err := copyFile(conn.dbPath, bkFilePath)
			if err != nil {
				return err
			}

			err = copyFile(conn.dbPath+"-wal", bkFilePath+"-wal")
			if err != nil {
				return err
			}

			err = copyFile(conn.dbPath+"-shm", bkFilePath+"-shm")
			if err != nil {
				return err
			}

			return nil
		})
	})

	if err != nil {
		return err
	}

	return conn.InstallCDC()
}

func (conn *SqliteStreamDB) GetRawConnection() *sqlite3.SQLiteConn {
	return conn.rawConnection
}

func (conn *SqliteStreamDB) GetPath() string {
	return conn.dbPath
}

func copyFile(toPath, fromPath string) error {
	fi, err := os.OpenFile(fromPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer fi.Close()

	fo, err := os.OpenFile(toPath, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	defer fo.Close()

	bytesWritten, err := io.Copy(fo, fi)
	log.Debug().
		Int64("bytes", bytesWritten).
		Str("from", fromPath).
		Str("to", toPath).
		Msg("copyFile")
	return err
}
