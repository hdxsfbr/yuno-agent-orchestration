-- Runs once when the pgdata volume is first created. Creates the test database used by
-- `make test`; the application schema is applied by the Go migrator at startup.
CREATE DATABASE yuno_test OWNER yuno;
