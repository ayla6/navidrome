-- +goose Up
SELECT 'up SQL query';

ALTER TABLE media_file ADD COLUMN source_track real;
ALTER TABLE media_file ADD COLUMN source_album real;

-- +goose Down
SELECT 'down SQL query';
