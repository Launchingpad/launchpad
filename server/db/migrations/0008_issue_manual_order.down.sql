-- Reverses 0008_issue_manual_order. The column is from 0001 and stays. The
-- backfill is NOT undone: the old ranks (0, or Linear's values) are not kept,
-- and the new ones are valid for the old schema, which ignores the column.
DROP INDEX IF EXISTS issues_manual_order_idx;
