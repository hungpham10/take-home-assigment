-- 0001_test_first.sql
CREATE TABLE tbl_migration_log (
    id   INT PRIMARY KEY,
    note TEXT
);
INSERT INTO tbl_migration_log VALUES (1, 'first');
