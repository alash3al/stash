-- +goose Up
ALTER TABLE namespaces ADD COLUMN persona TEXT DEFAULT '' NOT NULL;

-- +goose Down
ALTER TABLE namespaces DROP COLUMN persona;
