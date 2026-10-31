package index

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestRegistryRefusesTransactionControl(t *testing.T) {
	for _, sql := range []string{
		"BEGIN; CREATE TABLE a(x); COMMIT;",
		"begin transaction;\nCREATE TABLE a(x);",
		"CREATE TABLE a(x); commit",
		"CREATE TABLE a(x);\nEnd Transaction;",
		"CREATE TABLE a(x); ROLLBACK;",
		"SAVEPOINT s; CREATE TABLE a(x); RELEASE s;",
		"CREATE TABLE a(x); RELEASE SAVEPOINT s;",
		"-- set up\n  BEGIN IMMEDIATE; CREATE TABLE a(x);",
		"CREATE TRIGGER t AFTER INSERT ON a BEGIN INSERT INTO b VALUES (1); END; COMMIT;",
	} {
		_, err := LoadRegistry(fstest.MapFS{"0001-a.sql": {Data: []byte(sql)}})
		if err == nil || !strings.Contains(err.Error(), "0001-a.sql") {
			t.Errorf("%q: %v", sql, err)
		}
	}
	for _, sql := range []string{
		"CREATE TABLE a(x)",
		"CREATE TABLE a(x); -- COMMIT here would be wrong\n/* BEGIN; */ CREATE TABLE b(x);",
		"CREATE TABLE a(x); INSERT INTO a VALUES ('BEGIN; COMMIT;');",
		`CREATE TABLE "commit"("begin" TEXT, [end] TEXT, ` + "`rollback`" + ` TEXT);`,
		"CREATE TABLE a(x); CREATE TABLE b(y);\nCREATE TRIGGER t AFTER INSERT ON a BEGIN\n  UPDATE b SET y = CASE WHEN new.x > 0 THEN 1 ELSE 0 END;\n  INSERT INTO b VALUES (2);\nEND;",
		"CREATE TEMP TRIGGER t AFTER INSERT ON a BEGIN DELETE FROM a; END;",
		"CREATE TABLE a(x); SELECT CASE WHEN 1 THEN 2 END;",
	} {
		if _, err := LoadRegistry(fstest.MapFS{"0001-a.sql": {Data: []byte(sql)}}); err != nil {
			t.Errorf("%q refused: %v", sql, err)
		}
	}
}
