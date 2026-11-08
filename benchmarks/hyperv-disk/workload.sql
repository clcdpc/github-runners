-- Exclusive disposable benchmark SQL instance. Errors invalidate the run.
SET NOCOUNT ON;
SET XACT_ABORT ON;
IF DB_ID(N'ClcDiskBenchmark') IS NOT NULL THROW 51000, 'Stale benchmark database', 1;
DECLARE @start datetime2 = SYSUTCDATETIME();
CREATE DATABASE ClcDiskBenchmark
ON PRIMARY (NAME=N'ClcDiskBenchmark', FILENAME=N'/var/opt/mssql/data/ClcDiskBenchmark.mdf', SIZE=128MB, FILEGROWTH=64MB)
LOG ON (NAME=N'ClcDiskBenchmark_log', FILENAME=N'/var/opt/mssql/data/ClcDiskBenchmark_log.ldf', SIZE=64MB, FILEGROWTH=64MB);
ALTER DATABASE ClcDiskBenchmark SET RECOVERY SIMPLE;
ALTER DATABASE ClcDiskBenchmark SET DELAYED_DURABILITY = DISABLED;
DECLARE @initialized datetime2 = SYSUTCDATETIME();
EXEC(N'USE ClcDiskBenchmark;
  CREATE TABLE dbo.Writes (Id int NOT NULL PRIMARY KEY, Payload char(8000) NOT NULL);
  DECLARE @i int = 0;
  WHILE @i < 2000 BEGIN
    BEGIN TRANSACTION;
    INSERT dbo.Writes VALUES (@i, REPLICATE(''x'', 8000));
    COMMIT TRANSACTION;
    SET @i += 1;
  END;
  IF (SELECT COUNT_BIG(*) FROM dbo.Writes) <> 2000 THROW 51000, ''Write count mismatch'', 1;
  SELECT TOP (2000) Payload INTO #TempWrites FROM dbo.Writes;
  CHECKPOINT;');
DECLARE @written datetime2 = SYSUTCDATETIME();
-- Capture transient-file counters BEFORE dropping the database.
DECLARE @io nvarchar(max) = (SELECT f.type_desc AS file_kind, v.num_of_writes,
  v.num_of_bytes_written, v.io_stall_write_ms, v.num_of_reads, v.io_stall_read_ms
  FROM sys.dm_io_virtual_file_stats(DB_ID(N'ClcDiskBenchmark'), NULL) v
  JOIN sys.master_files f ON f.database_id=v.database_id AND f.file_id=v.file_id
  FOR JSON PATH);
DROP DATABASE ClcDiskBenchmark;
SELECT (SELECT DATEDIFF_BIG(microsecond,@start,@initialized)/1000000.0 AS database_init_seconds,
  DATEDIFF_BIG(microsecond,@initialized,@written)/1000000.0 AS writes_seconds,
  DATEDIFF_BIG(microsecond,@written,SYSUTCDATETIME())/1000000.0 AS drop_seconds,
  JSON_QUERY(@io) AS files FOR JSON PATH, WITHOUT_ARRAY_WRAPPER);
