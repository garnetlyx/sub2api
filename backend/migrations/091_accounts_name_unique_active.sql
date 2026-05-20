-- 091_accounts_name_unique_active.sql
-- Enforce unique active account names at the database layer.
-- Soft-deleted accounts are excluded so a deleted name can be reused.

CREATE UNIQUE INDEX IF NOT EXISTS accounts_name_unique_active
    ON accounts(name)
    WHERE deleted_at IS NULL;
